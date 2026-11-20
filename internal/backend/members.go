package backend

import (
	"cmp"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/i5shuyi/remote-go/internal/client"
	"github.com/i5shuyi/remote-go/internal/encoder"
	"github.com/i5shuyi/remote-go/internal/host"
	"github.com/i5shuyi/remote-go/internal/members"
	"github.com/i5shuyi/remote-go/internal/tunnel"
)

// "My devices": devices paired once connect to each other without device
// codes (docs/design/my-devices.md). The worker that hosts this App is the
// only writer of members.json; the GUI reads MembersDTO from State and
// answers local prompts through AnswerPrompt.

// MembersDTO is the device list and everything waiting on the person.
type MembersDTO struct {
	Ready        bool        `json:"ready"` // member state is open in this worker
	Error        string      `json:"error,omitempty"`
	InGroup      bool        `json:"inGroup"`
	Group        string      `json:"group,omitempty"` // short fingerprint
	Conflict     bool        `json:"conflict,omitempty"`
	AllowInbound bool        `json:"allowInbound"`
	Unattended   bool        `json:"unattended"`
	ViewOnly     bool        `json:"viewOnly"`
	Serving      bool        `json:"serving"` // member listener is up
	Devices      []DeviceDTO `json:"devices,omitempty"`
	Prompts      []PromptDTO `json:"prompts,omitempty"`
	Pairing      PairingDTO  `json:"pairing"`
	LastSync     time.Time   `json:"lastSync,omitzero"`
}

// DeviceDTO is one member.
type DeviceDTO struct {
	ID       string `json:"id"`
	Short    string `json:"short"`
	Name     string `json:"name"`
	Platform string `json:"platform"`
	Inbound  bool   `json:"inbound"` // false: controller only (Android)
	Self     bool   `json:"self"`
	Synced   bool   `json:"synced"` // has confirmed our current history
	KeyOK    bool   `json:"keyOK"`  // holds the current group key
}

// PromptDTO asks the person at this device to confirm something. The
// worker has paused remote input before it appears.
type PromptDTO struct {
	ID         int    `json:"id"`
	Kind       string `json:"kind"` // "pair" | "control" | "change"
	Code       string `json:"code,omitempty"`
	Peer       string `json:"peer"`
	PeerShort  string `json:"peerShort"`
	Role       string `json:"role,omitempty"` // pairing: inviter | joiner | recover
	NewGroup   bool   `json:"newGroup,omitempty"`
	Detail     string `json:"detail,omitempty"`
	CanInbound bool   `json:"canInbound,omitempty"` // offer "允许我的设备直连"
}

// PairingDTO reports the latest pairing on this device.
type PairingDTO struct {
	Busy   bool   `json:"busy"`
	Error  string `json:"error,omitempty"`
	Result string `json:"result,omitempty"`
}

type memberSide struct {
	changeMu sync.Mutex
	node     *members.Node
	store    *members.Store
	err      string
	tokens   members.TokenBook
	lns      memberListeners // guarded by memMu
	serving  bool            // guarded by mu
	pairLn   net.Listener
	pairStop context.CancelFunc
	prompts  map[int]*prompt
	seq      int
	pairing  PairingDTO
	lastSync time.Time
	syncNow  chan struct{}
}

type memberListener struct {
	ln  *tunnel.Listener
	srv *http.Server
}

type prompt struct {
	dto PromptDTO
	ch  chan [2]bool
}

// UIAttachedFunc reports whether a local window can show prompts. Without
// one, anything needing local confirmation is refused.
type UIAttachedFunc func() bool

func (a *App) memberNode() *members.Node {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.mem.node
}

// openMembers takes ownership of members.json. During a live update the
// old worker still holds the lock; keep trying until it hands it over.
func (a *App) openMembers(ctx context.Context) {
	a.mu.Lock()
	dir := filepath.Dir(a.cfgPath)
	a.mem.syncNow = make(chan struct{}, 1)
	a.mu.Unlock()
	if a.cfgPath == "" {
		a.setMemberErr("没有可用的配置目录")
		return
	}
	for {
		st, err := members.OpenStore(dir)
		if errors.Is(err, members.ErrLocked) {
			select {
			case <-ctx.Done():
				return
			case <-time.After(300 * time.Millisecond):
				continue
			}
		}
		if err != nil {
			a.setMemberErr(err.Error())
			return
		}
		n, err := members.OpenNode(st, keyStore(dir))
		if err != nil {
			st.Close()
			a.setMemberErr(err.Error())
			return
		}
		n.Approve = a.approveControl
		n.Changed = a.membersChanged
		a.mu.Lock()
		if ctx.Err() != nil {
			a.mu.Unlock()
			st.Close()
			return
		}
		a.mem.node, a.mem.store = n, st
		a.mu.Unlock()
		a.membersChanged()
		go a.syncLoop(ctx)
		return
	}
}

// keyStore is the private-directory fallback. TODO: macOS keychain,
// Windows DPAPI and Linux Secret Service behind the same interface.
func keyStore(dir string) members.KeyStore {
	return members.FileKeyStore{Dir: filepath.Join(dir, "member-keys")}
}

func (a *App) setMemberErr(e string) {
	a.mu.Lock()
	a.mem.err = e
	a.mu.Unlock()
	log.Printf("我的设备不可用：%s", e)
	a.emit()
}

func (a *App) closeMembers() {
	a.stopMemberListeners()
	a.mu.Lock()
	n, st := a.mem.node, a.mem.store
	a.mem.node, a.mem.store = nil, nil
	a.mu.Unlock()
	if n != nil {
		n.DisconnectAll()
	}
	if st != nil {
		st.Close() // hands the lock to the next worker
	}
}

func (a *App) membersChanged() {
	go a.reconcileMembers()
	a.emit()
}

// memberRelay describes the relay this desktop's member listener uses:
// the same one as its device code.
func (a *App) memberRelayLocked() *tunnel.MemberRelay {
	if a.cfg.RelayRegion == tunnel.CustomRegion {
		if a.cfg.RelayAddress == "" {
			return nil
		}
		return &tunnel.MemberRelay{Address: a.cfg.RelayAddress}
	}
	if !a.cfg.Homed {
		return nil
	}
	r, err := tunnel.Region(a.cfg.DeviceCode)
	if err != nil || r == tunnel.CustomRegion {
		return nil
	}
	return &tunnel.MemberRelay{Region: int(r)}
}

// reconcileMembers publishes this device's profile and runs exactly the
// member listeners the state asks for. Device-code changes never call it.
func (a *App) reconcileMembers() {
	a.memMu.Lock()
	defer a.memMu.Unlock()
	a.mu.Lock()
	n := a.mem.node
	on := a.cfg.AllowControl && a.net == "ready" && a.ctx.Err() == nil
	name, relay := a.cfg.Name, a.memberRelayLocked()
	a.mu.Unlock()
	if n == nil {
		return
	}
	if _, ok := n.Self(); ok && relay != nil {
		if err := n.SetProfile(name, runtime.GOOS, n.Policy().AllowInbound, relay); err != nil {
			log.Printf("更新本机自述失败：%v", err)
		}
	}
	n.Maintain()
	var endpoints []members.Endpoint
	if on {
		endpoints = n.ListenEndpoints()
	}
	if a.mem.lns == nil {
		a.mem.lns = memberListeners{}
	}
	a.mem.lns.reconcile(a.ctx, n, endpoints, a.serveMemberControl, a.logf())
	a.setMembersServing(len(a.mem.lns) > 0)
}

// EndpointHandler rechecks the address under the node's authorization lock,
// so a listener can stay up for sync when its key or relay stops being preferred.
type memberListeners map[string]*memberListener

func (ls memberListeners) reconcile(ctx context.Context, n *members.Node, endpoints []members.Endpoint, control members.ControlHandler, logf func(string, ...any)) error {
	want := make(map[string]members.Endpoint, len(endpoints))
	for _, ep := range endpoints {
		addr, err := ep.Addr()
		if err != nil {
			return err
		}
		want[string(addr)] = ep
	}
	for addr, l := range ls {
		if _, ok := want[addr]; !ok {
			l.close()
			delete(ls, addr)
		}
	}
	var firstErr error
	for addr, ep := range want {
		if ls[addr] != nil {
			continue
		}
		l, err := startMemberListener(ctx, n, ep, control, logf)
		if err != nil {
			log.Printf("成员监听启动失败：%v", err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		ls[addr] = l
	}
	return firstErr
}

func startMemberListener(parent context.Context, n *members.Node, ep members.Endpoint, control members.ControlHandler, logf func(string, ...any)) (*memberListener, error) {
	cfg, err := n.ServerTLS()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	ln, err := tunnel.ListenMember(ctx, ep.MemberEndpoint, logf)
	if err != nil {
		return nil, err
	}
	srv := &http.Server{Handler: n.EndpointHandler(ep, control), ReadHeaderTimeout: 10 * time.Second}
	go srv.Serve(tls.NewListener(ln, cfg))
	return &memberListener{ln: ln, srv: srv}, nil
}

func (l *memberListener) close() {
	l.srv.Close()
	l.ln.Close()
}

func (ls memberListeners) close() {
	for addr, l := range ls {
		l.close()
		delete(ls, addr)
	}
}

func (a *App) setMembersServing(serving bool) {
	a.mu.Lock()
	changed := a.mem.serving != serving
	a.mem.serving = serving
	a.mu.Unlock()
	if changed {
		a.emit()
	}
}

func (a *App) stopMemberListeners() {
	a.memMu.Lock()
	defer a.memMu.Unlock()
	a.mem.lns.close()
	a.setMembersServing(false)
}

func (a *App) serveMemberControl(ctx context.Context, conn net.Conn, peer members.MemberInfo) {
	a.mu.Lock()
	h := a.host
	n := a.mem.node
	a.mu.Unlock()
	if h == nil || n == nil {
		return
	}
	h.ServeMember(ctx, conn, host.MemberPeer{ID: peer.ID.String(), Name: peer.Name, ViewOnly: n.Policy().ViewOnly})
}

// ------------------------------------------------------------- prompts

// ask shows a prompt and waits. The input barrier is up before the
// prompt becomes visible and stays up until every prompt is answered.
func (a *App) ask(ctx context.Context, p PromptDTO) (ok, inbound bool) {
	if a.UIAttached != nil && !a.UIAttached() {
		return false, false
	}
	ch := make(chan [2]bool, 1)
	a.mu.Lock()
	if a.mem.prompts == nil {
		a.mem.prompts = map[int]*prompt{}
	}
	a.mem.seq++
	p.ID = a.mem.seq
	a.mem.prompts[p.ID] = &prompt{dto: p, ch: ch}
	a.setBarrierLocked()
	a.mu.Unlock()
	a.emit()
	defer func() {
		a.mu.Lock()
		delete(a.mem.prompts, p.ID)
		a.setBarrierLocked()
		a.mu.Unlock()
		a.emit()
	}()
	select {
	case r := <-ch:
		return r[0], r[1]
	case <-ctx.Done():
		return false, false
	}
}

func (a *App) setBarrierLocked() {
	if a.host != nil {
		a.host.PauseInput(len(a.mem.prompts) > 0)
	}
}

// AnswerPrompt records the person's local answer to a prompt.
func (a *App) AnswerPrompt(id int, ok, allowInbound bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	p, found := a.mem.prompts[id]
	if !found {
		return errors.New("该请求已失效")
	}
	select {
	case p.ch <- [2]bool{ok, allowInbound}:
	default:
	}
	return nil
}

func (a *App) approveControl(ctx context.Context, peer members.MemberInfo) bool {
	ok, _ := a.ask(ctx, PromptDTO{Kind: "control", Peer: peer.Name, PeerShort: peer.ID.Short()})
	return ok
}

func (a *App) pairOptions() members.PairOptions {
	a.mu.Lock()
	name, relay := a.cfg.Name, a.memberRelayLocked()
	a.mu.Unlock()
	return members.PairOptions{Name: name, Platform: runtime.GOOS, CanInbound: true, Relay: relay,
		Confirm: func(ctx context.Context, p members.PairPrompt) (bool, bool) {
			return a.ask(ctx, PromptDTO{Kind: "pair", Code: p.Code, Peer: p.PeerName, PeerShort: p.PeerID.Short(),
				Role: p.Role, NewGroup: p.NewGroup, CanInbound: p.Role == "joiner"})
		}}
}

// ------------------------------------------------------------- pairing

// pairToken is handed to a controller that proved the verification code.
func (a *App) pairToken() []byte {
	if a.memberNode() == nil {
		return nil
	}
	return a.mem.tokens.Issue("device-code")
}

// startPairServer accepts pairings on PairPort of the device-code tunnel.
func (a *App) startPairServer(ln *tunnel.Listener) {
	if a.memberNode() == nil {
		return
	}
	ctx, cancel := context.WithTimeout(a.ctx, 30*time.Second)
	pl, err := ln.ListenPort(ctx, tunnel.PairPort)
	cancel()
	if err != nil {
		log.Printf("配对端口不可用：%v", err)
		return
	}
	pctx, stop := context.WithCancel(a.ctx)
	a.mu.Lock()
	a.mem.pairLn, a.mem.pairStop = pl, stop
	a.mu.Unlock()
	go func() {
		for {
			c, err := pl.Accept()
			if err != nil {
				return
			}
			go a.finishPairing(func() error {
				n := a.memberNode()
				if n == nil {
					c.Close()
					return errors.New("我的设备不可用")
				}
				return n.PairServe(pctx, c, &a.mem.tokens, a.pairOptions())
			})
		}
	}()
}

func (a *App) stopPairServer() {
	a.mu.Lock()
	pl, stop := a.mem.pairLn, a.mem.pairStop
	a.mem.pairLn, a.mem.pairStop = nil, nil
	a.mu.Unlock()
	if stop != nil {
		stop() // ends pairings in progress, including their prompts
	}
	if pl != nil {
		pl.Close()
	}
}

func (a *App) finishPairing(run func() error) {
	a.mu.Lock()
	a.mem.pairing = PairingDTO{Busy: true}
	a.mu.Unlock()
	a.emit()
	err := run()
	a.mu.Lock()
	if err != nil {
		a.mem.pairing = PairingDTO{Error: err.Error()}
	} else {
		a.mem.pairing = PairingDTO{Result: "已添加到我的设备"}
	}
	done := a.mem.pairing
	a.mu.Unlock()
	if err == nil {
		a.SyncMembers()
	}
	a.emit()
	if err != nil {
		return // errors stay until the next attempt
	}
	// Success is a notice, not state: let it fade unless a newer attempt
	// has replaced it.
	time.AfterFunc(8*time.Second, func() {
		a.mu.Lock()
		cleared := a.mem.pairing == done
		if cleared {
			a.mem.pairing = PairingDTO{}
		}
		a.mu.Unlock()
		if cleared {
			a.emit()
		}
	})
}

// PairWith adds the device with these codes to "my devices". It runs in
// the background; progress and prompts appear in State().Members.
func (a *App) PairWith(code, password string) error {
	code, password = tunnel.Normalize(code), strings.TrimSpace(password)
	if !tunnel.Valid(code) {
		return errors.New("设备代码应为 9 位数字")
	}
	if password == "" {
		return errors.New("请输入验证码")
	}
	a.mu.Lock()
	n, endpoint, self := a.mem.node, a.cfg.RelayAddress, a.cfg.DeviceCode
	busy := a.mem.pairing.Busy
	a.mu.Unlock()
	switch {
	case n == nil:
		return errors.New("我的设备不可用")
	case code == self:
		return errors.New("不能添加本机")
	case busy:
		return errors.New("正在配对其他设备")
	}
	go a.finishPairing(func() error {
		ctx, cancel := context.WithTimeout(a.ctx, 3*time.Minute)
		defer cancel()
		return PairDial(ctx, n, code, password, endpoint, a.pairOptions(), a.logf())
	})
	return nil
}

// PairDial pairs with the device with these codes, for the desktop app and
// the phone app.
func PairDial(ctx context.Context, n *members.Node, code, password, endpoint string, opts members.PairOptions, logf func(string, ...any)) error {
	tc, err := tunnel.Dial(ctx, code, password, endpoint, logf)
	if err != nil {
		return err
	}
	defer tc.Close()
	hc := &http.Client{Transport: &http.Transport{DialContext: tc.DialContext, DisableKeepAlives: true}}
	var info struct {
		Members int `json:"members"`
	}
	if err := getJSON(ctx, hc, "GET", "http://remote-go/api/info", &info); err != nil {
		return err
	}
	if info.Members < 1 {
		return errors.New("对方版本不支持我的设备，请先更新对方")
	}
	var tok struct {
		Token []byte `json:"token"`
	}
	if err := getJSON(ctx, hc, "POST", "http://remote-go/api/pair-token?"+url.Values{"code": {password}}.Encode(), &tok); err != nil {
		return err
	}
	if len(tok.Token) == 0 {
		return errors.New("对方的我的设备不可用")
	}
	conn, err := tc.DialPort(ctx, tunnel.PairPort)
	if err != nil {
		return err
	}
	return n.PairDial(ctx, conn, tok.Token, opts)
}

func getJSON(ctx context.Context, hc *http.Client, method, u string, v any) error {
	req, _ := http.NewRequestWithContext(ctx, method, u, nil)
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return client.ErrBadCode
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return fmt.Errorf("对方拒绝：%s", b)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(v)
}

// ------------------------------------------------------------- dialing

const reachTimeout = 15 * time.Second

// DialMember reaches a member's preferred address, syncs and asks for
// control. Non-preferred addresses are used only to catch up.
func DialMember(ctx context.Context, n *members.Node, id members.ID, logf func(string, ...any)) (net.Conn, *tunnel.Conn, error) {
	eps := n.DialEndpoints(id)
	if len(eps) == 0 {
		return nil, nil, errors.New("等待同步：还不知道该设备的地址或本机尚无当前组密钥")
	}
	var last error
	for pass := 0; pass < 2; pass++ {
		for _, ep := range eps {
			if !ep.Control && pass == 0 {
				continue
			}
			// Reaching an address is bounded on its own, so one offline
			// address cannot use up the time meant for local approval.
			rctx, rcancel := context.WithTimeout(ctx, reachTimeout)
			tc, err := tunnel.DialMember(rctx, ep.MemberEndpoint, logf)
			if err != nil {
				rcancel()
				last = err
				continue
			}
			raw, err := tc.DialContext(rctx, "tcp", "")
			rcancel()
			if err != nil {
				tc.Close()
				last = err
				continue
			}
			if !ep.Control {
				_, last = n.DialSync(ctx, raw, id)
				tc.Close()
				continue
			}
			conn, err := n.DialControl(ctx, raw, id)
			if err != nil {
				tc.Close()
				return nil, nil, err
			}
			return conn, tc, nil
		}
		if pass == 0 {
			eps = n.DialEndpoints(id) // a catch-up sync may have taught us more
		}
	}
	if last == nil {
		last = errors.New("无法联系设备")
	}
	return nil, nil, last
}

// ConnectMember opens a remote session to a member, in this process.
func ConnectMember(ctx context.Context, n *members.Node, idStr string, cfg client.Config, h client.Handler, logf func(string, ...any)) (*Remote, error) {
	id, err := members.ParseID(idStr)
	if err != nil {
		return nil, err
	}
	conn, tc, err := DialMember(ctx, n, id, logf)
	if err != nil {
		return nil, err
	}
	r, err := sessionOver(ctx, conn, tc, cfg, h)
	if err != nil {
		return nil, err
	}
	r.Target.Member = idStr
	return r, nil
}

// sessionOver runs the desktop protocol on an approved member connection.
func sessionOver(ctx context.Context, conn net.Conn, tc *tunnel.Conn, cfg client.Config, h client.Handler) (*Remote, error) {
	var once sync.Once
	dial := func(context.Context, string, string) (net.Conn, error) {
		var c net.Conn
		once.Do(func() { c = conn })
		if c == nil {
			return nil, errors.New("成员连接只能使用一次")
		}
		return c, nil
	}
	onClose := h.OnClose
	h.OnClose = func(err error) {
		if tc != nil {
			tc.Close()
		}
		if onClose != nil {
			onClose(err)
		}
	}
	s, err := client.Dial(ctx, dial, fmt.Sprintf("remote-go:%d", tunnel.MemberPort), "", encoder.CanDecodeH264(), cfg, h)
	if err != nil {
		conn.Close()
		if tc != nil {
			tc.Close()
		}
		return nil, err
	}
	return &Remote{Target: Target{Control: true}, Session: s, Tunnel: tc}, nil
}

// ConnectMember opens a session to a member from the in-process GUI.
func (a *App) ConnectMember(id string, cfg client.Config, h client.Handler) (*Remote, error) {
	n := a.memberNode()
	if n == nil {
		return nil, errors.New("我的设备不可用")
	}
	ctx, cancel := context.WithTimeout(a.ctx, members.ApproveTimeout+30*time.Second)
	defer cancel()
	r, err := ConnectMember(ctx, n, id, cfg, h, a.logf())
	if r != nil {
		r.Target.Name = a.memberName(id)
	}
	return r, err
}

func (a *App) memberName(id string) string {
	for _, d := range a.membersDTO().Devices {
		if d.ID == id {
			return d.Name
		}
	}
	return "我的设备"
}

// MemberProxy dials a member and exposes the approved connection on a
// one-shot loopback port guarded by a random token, so a separate session
// window process can run the desktop protocol over it.
func (a *App) MemberProxy(idStr string) (addr, token, name string, err error) {
	n := a.memberNode()
	if n == nil {
		return "", "", "", errors.New("我的设备不可用")
	}
	id, err := members.ParseID(idStr)
	if err != nil {
		return "", "", "", err
	}
	ctx, cancel := context.WithTimeout(a.ctx, members.ApproveTimeout+20*time.Second)
	defer cancel()
	conn, tc, err := DialMember(ctx, n, id, a.logf())
	if err != nil {
		return "", "", "", err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		conn.Close()
		tc.Close()
		return "", "", "", err
	}
	tok := make([]byte, 32)
	rand.Read(tok)
	token = hex.EncodeToString(tok)
	go func() {
		defer tc.Close()
		defer conn.Close()
		defer ln.Close()
		ln.(*net.TCPListener).SetDeadline(time.Now().Add(30 * time.Second))
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.SetReadDeadline(time.Now().Add(5 * time.Second))
			got := make([]byte, len(token))
			if _, err := io.ReadFull(c, got); err != nil || subtle.ConstantTimeCompare(got, []byte(token)) != 1 {
				c.Close()
				continue
			}
			c.SetReadDeadline(time.Time{})
			ln.Close()
			go func() { io.Copy(conn, c); conn.Close() }()
			io.Copy(c, conn)
			c.Close()
			return
		}
	}()
	return ln.Addr().String(), token, a.memberName(idStr), nil
}

// DialProxy connects to a MemberProxy from the session window process.
func DialProxy(ctx context.Context, addr, token string, cfg client.Config, h client.Handler) (*Remote, error) {
	var d net.Dialer
	c, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	if _, err := io.WriteString(c, token); err != nil {
		c.Close()
		return nil, err
	}
	return sessionOver(ctx, c, nil, cfg, h)
}

// ------------------------------------------------------------- sync and ops

// SyncMembers asks the background loop to sync with reachable members now.
func (a *App) SyncMembers() {
	a.mu.Lock()
	ch := a.mem.syncNow
	a.mu.Unlock()
	if ch != nil {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (a *App) syncLoop(ctx context.Context) {
	a.mu.Lock()
	ch := a.mem.syncNow
	a.mu.Unlock()
	t := time.NewTimer(2 * time.Second)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-ch:
		}
		if n := a.memberNode(); n != nil {
			a.reconcileMembers()
			SyncAll(ctx, n, a.logf())
			a.mu.Lock()
			a.mem.lastSync = time.Now()
			a.mu.Unlock()
			a.emit()
		}
		t.Reset(3 * time.Minute)
	}
}

// SyncAll syncs with every reachable member that accepts connections,
// a few at a time. Unreachable members simply stay pending.
func SyncAll(ctx context.Context, n *members.Node, logf func(string, ...any)) {
	sem := make(chan struct{}, 3)
	var wg sync.WaitGroup
	for _, m := range n.Snapshot().Members {
		if m.Self || !m.Inbound {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(id members.ID) {
			defer wg.Done()
			defer func() { <-sem }()
			for _, ep := range n.DialEndpoints(id) {
				c, cancel := context.WithTimeout(ctx, 20*time.Second)
				err := syncOnce(c, n, id, ep, logf)
				cancel()
				if err == nil || errors.Is(err, members.ErrFork) {
					return
				}
			}
		}(m.ID)
	}
	wg.Wait()
}

func syncOnce(ctx context.Context, n *members.Node, id members.ID, ep members.Endpoint, logf func(string, ...any)) error {
	tc, err := tunnel.DialMember(ctx, ep.MemberEndpoint, logf)
	if err != nil {
		return err
	}
	defer tc.Close()
	raw, err := tc.DialContext(ctx, "tcp", "")
	if err != nil {
		return err
	}
	_, err = n.DialSync(ctx, raw, id)
	return err
}

func (a *App) confirmMemberChange(detail string, fn func(*members.Node) error) error {
	if !a.mem.changeMu.TryLock() {
		return errors.New("另一个设备组修改正在等待处理")
	}
	defer a.mem.changeMu.Unlock()
	n := a.memberNode()
	if n == nil {
		return errors.New("我的设备不可用")
	}
	ctx, cancel := context.WithTimeout(a.ctx, members.ApproveTimeout)
	defer cancel()
	ok, _ := a.ask(ctx, PromptDTO{Kind: "change", Detail: detail})
	if !ok || ctx.Err() != nil {
		return errors.New("未在本机确认修改")
	}
	err := fn(n)
	a.SyncMembers()
	return err
}

// RemoveMember removes a device; rotate also replaces the group key.
func (a *App) RemoveMember(idStr string, rotate bool) error {
	id, err := members.ParseID(idStr)
	if err != nil {
		return err
	}
	detail := fmt.Sprintf("从设备组移除「%s」（%s）", a.memberName(idStr), id.Short())
	if rotate {
		detail += "，并更换组密钥"
	}
	return a.confirmMemberChange(detail, func(n *members.Node) error { _, err := n.Remove(id, rotate); return err })
}

// RotateMemberKey replaces the group key.
func (a *App) RotateMemberKey() error {
	return a.confirmMemberChange("更换设备组密钥", func(n *members.Node) error { _, err := n.Rotate(); return err })
}

// SetMemberPolicy changes "允许我的设备直连", unattended access and
// view-only for member sessions.
func (a *App) SetMemberPolicy(p members.Policy) error {
	state := func(on bool) string {
		if on {
			return "开启"
		}
		return "关闭"
	}
	detail := fmt.Sprintf("允许我的设备直连：%s；无人值守：%s；仅观看：%s", state(p.AllowInbound), state(p.Unattended), state(p.ViewOnly))
	return a.confirmMemberChange(detail, func(n *members.Node) error { return n.SetPolicy(p) })
}

// LeaveMembers removes this device from its group, tells reachable
// members, and deletes the local identity and keys.
func (a *App) LeaveMembers() error {
	return a.confirmMemberChange("退出设备组，通知可达设备后清除本机身份和密钥", func(n *members.Node) error {
		if _, err := n.Leave(); err != nil && !errors.Is(err, members.ErrNotMember) {
			return err
		}
		ctx, cancel := context.WithTimeout(a.ctx, 30*time.Second)
		defer cancel()
		SyncAll(ctx, n, a.logf())
		return n.Forget()
	})
}

// RebuildMembers stops using a conflicted (or broken) group locally; pair
// the devices to keep again afterwards.
func (a *App) RebuildMembers() error {
	return a.confirmMemberChange("停用旧设备组并清除本机身份和密钥", func(n *members.Node) error { return n.Forget() })
}

func (a *App) membersDTO() MembersDTO {
	a.mu.Lock()
	n := a.mem.node
	d := MembersDTO{Error: a.mem.err, Pairing: a.mem.pairing, LastSync: a.mem.lastSync, Serving: a.mem.serving}
	for _, p := range a.mem.prompts {
		d.Prompts = append(d.Prompts, p.dto)
	}
	a.mu.Unlock()
	slices.SortFunc(d.Prompts, func(a, b PromptDTO) int { return cmp.Compare(a.ID, b.ID) })
	if n == nil {
		return d
	}
	d.Ready = true
	s := n.Snapshot()
	if s.Err != nil {
		d.Error = s.Err.Error()
	}
	d.InGroup, d.Conflict = s.InGroup, s.Conflict
	if !s.Group.IsZero() {
		d.Group = s.Group.Short()
	}
	d.AllowInbound, d.Unattended, d.ViewOnly = s.Policy.AllowInbound, s.Policy.Unattended, s.Policy.ViewOnly
	for _, m := range s.Members {
		name := m.Name
		if name == "" {
			name = "设备 " + m.ID.Short()
		}
		d.Devices = append(d.Devices, DeviceDTO{ID: m.ID.String(), Short: m.ID.Short(), Name: name, Platform: m.Platform,
			Inbound: m.Inbound, Self: m.Self, Synced: m.Synced, KeyOK: m.KeyVersion == s.KeyVersion})
	}
	return d
}

// ServeMembers serves "my devices" for the command-line host with the
// identity paired by the desktop app. There is no local confirmation UI,
// so control works only where unattended access was turned on.
func ServeMembers(ctx context.Context, h *host.Server, logf func(string, ...any)) error {
	base, err := userConfigDir()
	if err != nil {
		return err
	}
	st, err := members.OpenStore(base)
	if errors.Is(err, members.ErrLocked) {
		return errors.New("桌面应用正在使用我的设备，请先退出它")
	}
	if err != nil {
		return err
	}
	defer st.Close()
	n, err := members.OpenNode(st, keyStore(base))
	if err != nil {
		return err
	}
	return serveMemberNode(ctx, n, h, logf)
}

func serveMemberNode(ctx context.Context, n *members.Node, h *host.Server, logf func(string, ...any)) error {
	if err := n.Err(); err != nil {
		return err
	}
	if !n.Snapshot().InGroup || !n.Policy().AllowInbound {
		return errors.New("本机未加入设备组或未开启「允许我的设备直连」，请先在桌面应用中配对")
	}
	if len(n.ListenEndpoints()) == 0 {
		return errors.New("本机还没有成员地址：请先在桌面应用中开启远程控制并完成一次同步")
	}
	changed := make(chan struct{}, 1)
	n.Changed = func() {
		select {
		case changed <- struct{}{}:
		default:
		}
	}
	control := func(ctx context.Context, conn net.Conn, peer members.MemberInfo) {
		h.ServeMember(ctx, conn, host.MemberPeer{ID: peer.ID.String(), Name: peer.Name, ViewOnly: n.Policy().ViewOnly})
	}
	listeners := memberListeners{}
	defer listeners.close()
	defer n.DisconnectAll()
	refresh := func() error {
		if err := n.Maintain(); err != nil {
			return err
		}
		return listeners.reconcile(ctx, n, n.ListenEndpoints(), control, logf)
	}
	if err := refresh(); err != nil && len(listeners) == 0 {
		return err
	}
	mode := "每次连接需本机确认，命令行无法确认，因此会被拒绝"
	if n.Policy().Unattended {
		mode = "无人值守"
	}
	log.Printf("我的设备：已在 %d 个成员地址上监听（%s）", len(listeners), mode)
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(3 * time.Minute)
		defer t.Stop()
		for {
			SyncAll(ctx, n, logf)
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				n.Changed() // expire transitions and retry failed listeners even with no peers
			}
		}
	}()
	for {
		select {
		case <-changed:
			if err := refresh(); err != nil {
				log.Printf("更新成员监听失败：%v", err)
			}
		case <-ctx.Done():
			<-done
			return nil
		}
	}
}

func userConfigDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "remote-go"), nil
}
