package members

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// Routes on the member listener. Each upgrades to a framed stream; a
// connection keeps the purpose it was opened with.
const (
	SyncPath    = "/members/v1/sync"
	ControlPath = "/members/v1/control"
	upgradeName = "remote-go-members-v1"
)

// ApproveTimeout bounds how long a control request waits for the person.
const ApproveTimeout = 30 * time.Second

// ApproveFunc shows the authenticated device name at this desktop and
// returns true only if the person allows it before ctx ends.
type ApproveFunc func(ctx context.Context, peer MemberInfo) bool

type syncHead struct {
	Group ID `json:"group"`
	Head  ID `json:"head"`
}

type syncData struct {
	Records  []Signed  `json:"records,omitempty"`
	Conflict *Conflict `json:"conflict,omitempty"`
	Profiles []Signed  `json:"profiles,omitempty"`
	Acks     []Signed  `json:"acks,omitempty"`
	Receipts []Signed  `json:"receipts,omitempty"`
}

type syncKey struct {
	Version *ID `json:"version,omitempty"`
	Key     B64 `json:"key,omitempty"`
}

type syncFinal struct {
	Ack      *Signed  `json:"ack,omitempty"`
	Receipts []Signed `json:"receipts,omitempty"`
}

type controlResult struct {
	OK     bool   `json:"ok"`
	Reason string `json:"reason,omitempty"`
}

// SyncResult summarizes one sync.
type SyncResult struct {
	Added       int
	Removed     []ID
	KeyReceived bool
	KeySent     bool
}

func (n *Node) myHead() (syncHead, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.g == nil {
		return syncHead{}, errors.New("本机未加入设备组")
	}
	return syncHead{Group: n.g.ID(), Head: n.g.Head()}, nil
}

func (n *Node) dataFor(peerHead ID) syncData {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.g == nil {
		return syncData{}
	}
	d := syncData{Records: n.g.Suffix(peerHead), Conflict: n.g.Conflict()}
	d.Profiles, d.Acks, d.Receipts = n.g.Extras()
	return d
}

func (n *Node) applyData(peer ID, d syncData, res *SyncResult) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.g == nil {
		return errors.New("本机未加入设备组")
	}
	var forked error
	if n.g.Conflict() == nil {
		r, err := n.g.Apply(d.Records)
		switch {
		case errors.Is(err, ErrFork):
			forked = err
		case err != nil:
			return fmt.Errorf("拒绝同步的成员历史：%w", err)
		}
		res.Added, res.Removed = r.Added, r.Removed
	}
	if d.Conflict != nil && n.g.Conflict() == nil {
		if err := n.g.ApplyConflict(*d.Conflict); errors.Is(err, ErrFork) {
			forked = err
		}
	}
	for _, r := range d.Profiles {
		n.g.ApplyProfile(r)
	}
	for _, r := range d.Acks {
		n.g.ApplyAck(r)
	}
	for _, r := range d.Receipts {
		n.g.ApplyReceipt(r)
	}
	if err := n.commitLocked(nil); err != nil {
		return err
	}
	if forked != nil {
		return forked
	}
	return nil
}

func (n *Node) newAck() *Signed {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.usableLocked() != nil || n.g.Conflict() != nil {
		return nil
	}
	held := n.f.KeyCurrent
	if _, ok := n.k[held]; !ok {
		held = ID{}
	}
	a, err := n.g.NewAck(n.self, held)
	if err != nil {
		return nil
	}
	return &a
}

func (n *Node) applyAck(peer ID, a *Signed) (Ack, bool) {
	if a == nil {
		return Ack{}, false
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.g == nil {
		return Ack{}, false
	}
	ack, err := n.g.ApplyAck(*a)
	if err != nil || ack.Member != peer {
		return Ack{}, false
	}
	n.commitLocked(nil)
	return ack, true
}

// receiptFor confirms the peer's current profile, for relay migration.
func (n *Node) receiptFor(peer ID) []Signed {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.usableLocked() != nil {
		return nil
	}
	if _, ok := n.g.ProfileRecord(peer); !ok {
		return nil
	}
	r, err := n.g.NewReceipt(n.self, peer)
	if err != nil {
		return nil
	}
	n.commitLocked(nil)
	return []Signed{r}
}

// sync runs the symmetric protocol on an authenticated stream:
// heads → missing history and discovery records → acks → key → acks.
func (n *Node) sync(fr *frames, peer ID) (SyncResult, error) {
	var res SyncResult
	mine, err := n.myHead()
	if err != nil {
		return res, err
	}
	var theirs syncHead
	if err := fr.exchange(mine, &theirs); err != nil {
		return res, err
	}
	if theirs.Group != mine.Group {
		return res, errors.New("对方属于其他设备组")
	}
	var in syncData
	if err := fr.exchange(n.dataFor(theirs.Head), &in); err != nil {
		return res, err
	}
	if err := n.applyData(peer, in, &res); err != nil {
		return res, err
	}
	var peerAck syncFinal
	if err := fr.exchange(syncFinal{Ack: n.newAck()}, &peerAck); err != nil {
		return res, err
	}
	ack, ok := n.applyAck(peer, peerAck.Ack)

	var out syncKey
	n.mu.Lock()
	if ok {
		// Removals in the history were applied above; re-check the
		// receiver under the current state right before sending.
		if v, k, give := n.keyForLocked(peer, ack); give {
			out = syncKey{Version: &v, Key: k}
		}
	}
	n.mu.Unlock()
	var inKey syncKey
	if err := fr.exchange(out, &inKey); err != nil {
		return res, err
	}
	res.KeySent = out.Version != nil
	if inKey.Version != nil {
		n.mu.Lock()
		err := n.installKeyLocked(*inKey.Version, inKey.Key)
		n.mu.Unlock()
		if err != nil {
			return res, err
		}
		res.KeyReceived = true
	}
	var final syncFinal
	if err := fr.exchange(syncFinal{Ack: n.newAck(), Receipts: n.receiptFor(peer)}, &final); err != nil {
		return res, err
	}
	finalAck, acknowledged := n.applyAck(peer, final.Ack)
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, r := range final.Receipts {
		if n.g != nil {
			n.g.ApplyReceipt(r)
		}
	}
	if n.g == nil {
		return res, ErrNotMember
	}
	if err := n.commitLocked(nil); err != nil {
		return res, err
	}
	if n.g.Conflict() == nil {
		state := n.g.State()
		if _, held := n.k[state.KeyVersion]; !held || !acknowledged || finalAck.KeyVersion != state.KeyVersion || finalAck.Head != state.Head {
			return res, errors.New("等待同步：双方尚未确认当前历史和组密钥")
		}
	}
	return res, nil
}

// --- server ---

// ControlHandler runs the desktop protocol on an approved, registered
// connection. It returns when the session ends; ctx is cancelled on
// revocation.
type ControlHandler func(ctx context.Context, conn net.Conn, peer MemberInfo)

// Handler serves the member routes. The listener must wrap connections
// with ServerTLS.
func (n *Node) Handler(control ControlHandler) http.Handler {
	return n.handler(nil, control)
}

// EndpointHandler binds control authorization to the address that received the request.
func (n *Node) EndpointHandler(ep Endpoint, control ControlHandler) http.Handler {
	return n.handler(&ep, control)
}

func (n *Node) handler(ep *Endpoint, control ControlHandler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+SyncPath, func(w http.ResponseWriter, r *http.Request) { n.serve(w, r, PurposeSync, nil, nil) })
	mux.HandleFunc("GET "+ControlPath, func(w http.ResponseWriter, r *http.Request) { n.serve(w, r, PurposeControl, ep, control) })
	return mux
}

func (n *Node) serve(w http.ResponseWriter, r *http.Request, purpose string, ep *Endpoint, control ControlHandler) {
	if r.TLS == nil || r.Header.Get("Upgrade") != upgradeName {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	pub, err := PeerKey(*r.TLS)
	if err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	peer, err := n.Authorize(pub, purpose, true)
	if err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	conn, brw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		return
	}
	defer conn.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	release, err := n.Register(peer, PurposeSync, true, func() { cancel(); conn.Close() })
	if err != nil {
		return
	}
	io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: "+upgradeName+"\r\nConnection: Upgrade\r\n\r\n")
	deadline(conn, handshakeTime)
	fr := newFrames(brw.Reader, conn)
	_, serr := n.sync(fr, peer)
	release()
	if purpose != PurposeControl {
		return
	}
	reject := func(reason string) { fr.send(controlResult{Reason: reason}) }
	if serr != nil {
		reject("等待同步")
		return
	}
	n.serveControl(ctx, cancel, conn, brw.Reader, fr, peer, ep, control, reject)
}

func (n *Node) serveControl(ctx context.Context, cancel context.CancelFunc, conn net.Conn, br *bufio.Reader, fr *frames,
	peer ID, ep *Endpoint, control ControlHandler, reject func(string)) {
	if control == nil {
		reject("被拒绝")
		return
	}
	// Pending approval is itself a registered connection, so a removal or
	// closing inbound cancels it.
	s := &session{member: peer, purpose: PurposeControl, inbound: true, endpoint: ep, cancel: func() { cancel(); conn.Close() }}
	release, err := n.register(s)
	if err != nil {
		reject(err.Error())
		return
	}
	info := n.memberInfo(peer)
	if !n.Policy().Unattended {
		if n.Approve == nil {
			release()
			reject("目标桌面无法显示确认界面")
			return
		}
		deadline(conn, ApproveTimeout+5*time.Second)
		actx, acancel := context.WithTimeout(ctx, ApproveTimeout)
		ok := n.Approve(actx, info)
		acancel()
		if !ok || ctx.Err() != nil {
			release()
			reject("被拒绝")
			return
		}
	}
	release()
	// A late approval after revocation fails here.
	release, err = n.register(s)
	if err != nil {
		reject(err.Error())
		return
	}
	defer release()
	if fr.send(controlResult{OK: true}) != nil {
		return
	}
	deadline(conn, 0)
	control(ctx, &bufConn{Conn: conn, r: br}, info)
}

func (n *Node) memberInfo(id ID) MemberInfo {
	n.mu.Lock()
	defer n.mu.Unlock()
	m := MemberInfo{ID: id}
	if n.g != nil {
		if p, ok := n.g.Profile(id); ok {
			m.Name, m.Platform, m.Inbound = p.Name, p.Platform, p.Inbound
		}
	}
	return m
}

type bufConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufConn) Read(b []byte) (int, error) { return c.r.Read(b) }

// --- client ---

func (n *Node) upgrade(ctx context.Context, raw net.Conn, target ID, purpose, path string) (*tls.Conn, *frames, *bufio.Reader, error) {
	cfg, err := n.ClientTLS(target, purpose)
	if err != nil {
		return nil, nil, nil, err
	}
	deadline(raw, handshakeTime)
	tc := tls.Client(raw, cfg)
	if err := tc.HandshakeContext(ctx); err != nil {
		return nil, nil, nil, fmt.Errorf("身份不匹配或无法建立安全连接：%w", err)
	}
	fmt.Fprintf(tc, "GET %s HTTP/1.1\r\nHost: remote-go-member\r\nUpgrade: %s\r\nConnection: Upgrade\r\n\r\n", path, upgradeName)
	br := bufio.NewReader(tc)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		return nil, nil, nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		return nil, nil, nil, fmt.Errorf("被拒绝：%s", string(b))
	}
	return tc, newFrames(br, tc), br, nil
}

// DialSync syncs with target over raw (a tunnel connection to one of its
// endpoints).
func (n *Node) DialSync(ctx context.Context, raw net.Conn, target ID) (SyncResult, error) {
	defer raw.Close()
	stop := context.AfterFunc(ctx, func() { raw.Close() })
	defer stop()
	tc, fr, _, err := n.upgrade(ctx, raw, target, PurposeSync, SyncPath)
	if err != nil {
		return SyncResult{}, err
	}
	defer tc.Close()
	return n.sync(fr, target)
}

// DialControl syncs, then asks target for control. On success the
// returned connection carries the desktop protocol.
func (n *Node) DialControl(ctx context.Context, raw net.Conn, target ID) (net.Conn, error) {
	release, err := n.Register(target, PurposeControl, false, func() { raw.Close() })
	if err != nil {
		raw.Close()
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { raw.Close() })
	defer stop()
	tracked := &registeredConn{Conn: raw, release: release}
	tc, fr, br, err := n.upgrade(ctx, tracked, target, PurposeControl, ControlPath)
	if err != nil {
		tracked.Close()
		return nil, err
	}
	if _, err := n.sync(fr, target); err != nil {
		tc.Close()
		return nil, fmt.Errorf("等待同步：%w", err)
	}
	n.mu.Lock()
	err = n.authorizedLocked(target, PurposeControl, false)
	n.mu.Unlock()
	if err != nil {
		tc.Close()
		return nil, err
	}
	deadline(tc, ApproveTimeout+10*time.Second)
	var res controlResult
	if err := fr.recv(&res); err != nil {
		tc.Close()
		return nil, err
	}
	if !res.OK {
		tc.Close()
		return nil, fmt.Errorf("被拒绝：%s", res.Reason)
	}
	deadline(tc, 0)
	return &bufConn{Conn: tc, r: br}, nil
}

// registeredConn releases the node's authorization entry when TLS closes the transport.
type registeredConn struct {
	net.Conn
	release func()
}

func (c *registeredConn) Close() error {
	c.release()
	return c.Conn.Close()
}
