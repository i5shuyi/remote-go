// Package backend is the desktop app's logic, independent of its UI: the
// device is both controlled (device code + verification code) and a
// controller (enter the other side's codes to connect).
//
// Devices meet through tailcat (package tunnel): Tailscale's WireGuard data
// plane and DERP relays, with no account, login or server of our own. The
// host's tunnel key is derived from its device code and verification code,
// so whoever knows both can compute its address and nobody else can.
package backend

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"tailscale.com/atomicfile"
	"tailscale.com/tailcfg"

	"github.com/i5shuyi/remote-go/internal/client"
	"github.com/i5shuyi/remote-go/internal/encoder"
	"github.com/i5shuyi/remote-go/internal/host"
	"github.com/i5shuyi/remote-go/internal/tunnel"
)

// Config is persisted to <UserConfigDir>/remote-go/app.json.
type Config struct {
	DeviceCode    string   `json:"deviceCode"`
	Homed         bool     `json:"homed"`                 // the code has been assigned to the selected relay
	RelayRegion   int      `json:"relayRegion,omitempty"` // zero selects the nearest public region
	RelayAddress  string   `json:"relayAddress,omitempty"`
	Name          string   `json:"name"`
	FixedPassword string   `json:"fixedPassword,omitempty"`
	AllowControl  bool     `json:"allowControl"`
	Quality       int      `json:"quality,omitempty"`
	FPS           int      `json:"fps,omitempty"`
	Recent        []Recent `json:"recent,omitempty"`
}

// Recent is a device this one connected to.
type Recent struct {
	Code string    `json:"code"`
	Name string    `json:"name"`
	OS   string    `json:"os"`
	When time.Time `json:"when"`
}

// App is the running backend. Its methods are safe for concurrent use and
// may block on the network: call them off the UI thread.
type App struct {
	ctx    context.Context
	cancel context.CancelFunc

	// OnChange is called (on any goroutine) whenever State changes.
	OnChange func()
	// Verbose logs tailcat's diagnostics.
	Verbose bool

	mu       sync.Mutex
	cfg      Config
	cfgPath  string
	password string // current verification code
	net      string // starting | ready | error
	netErr   string

	relayMu      sync.Mutex // serializes saving relay settings and replacing the listener
	cancelHoming context.CancelFunc

	host    *host.Server
	hostLn  *tunnel.Listener
	hostErr string
	hostGen int // bumps on every start/stop, so a slow start can tell it is stale

	mem   memberSide
	memMu sync.Mutex // serializes member listener changes
	// UIAttached, if set, reports whether a window can show local prompts.
	UIAttached UIAttachedFunc
}

func New() *App { return &App{net: "starting"} }

// ------------------------------------------------------------- lifecycle

// Start loads the configuration and, in the background, finds the nearest
// relay and starts listening.
func (a *App) Start() { a.StartWithPassword("") }

// StartWithPassword carries the current random code across an idle worker
// replacement. A normal application launch still generates a fresh code.
func (a *App) StartWithPassword(previous string) {
	a.ctx, a.cancel = context.WithCancel(context.Background())
	if err := a.loadConfig(); err != nil {
		log.Printf("读取配置失败: %v", err)
	}
	a.password = a.cfg.FixedPassword
	if a.password == "" {
		a.password = previous
	}
	if a.password == "" {
		a.password = randomPassword()
	}
	ctx, cancel := context.WithCancel(a.ctx)
	a.cancelHoming = cancel
	go a.openMembers(a.ctx)
	go a.bringUp(ctx)
}

// DrainIfIdle closes admission atomically with testing for active viewers.
// Cancelling startup also prevents an in-flight listener from accepting one.
func (a *App) DrainIfIdle() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.host != nil && !a.host.DrainIfIdle() {
		return false
	}
	a.cancel()
	a.hostGen++
	return true
}

// Stop shuts everything down and releases members.json for the next
// worker.
func (a *App) Stop() {
	if a.cancel != nil {
		a.cancel()
	}
	a.stopAll()
	a.closeMembers()
}

func (a *App) loadConfig() error {
	base, err := os.UserConfigDir()
	if err != nil {
		return err
	}
	a.cfgPath = filepath.Join(base, "remote-go", "app.json")
	a.cfg = Config{AllowControl: true, Quality: 70, FPS: 30}
	if b, err := os.ReadFile(a.cfgPath); err == nil {
		json.Unmarshal(b, &a.cfg)
	}
	if a.cfg.Name == "" {
		a.cfg.Name, _ = os.Hostname()
		a.cfg.Name = strings.TrimSuffix(a.cfg.Name, ".local")
		return a.saveConfig()
	}
	return nil
}

func (a *App) saveConfig() error {
	if err := os.MkdirAll(filepath.Dir(a.cfgPath), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(a.cfg, "", "  ")
	return atomicfile.WriteFile(a.cfgPath, b, 0o600)
}

func (a *App) emit() {
	if a.OnChange != nil {
		a.OnChange()
	}
}

func (a *App) logf() func(string, ...any) { return tunnel.Logf(a.Verbose) }

// bringUp assigns the configured relay, retaining a previously selected public
// region in automatic mode so the device code is stable across restarts.
func (a *App) bringUp(ctx context.Context) {
	for {
		a.mu.Lock()
		if ctx.Err() != nil {
			a.mu.Unlock()
			return
		}
		code, region := a.cfg.DeviceCode, tailcfg.DERPRegionID(a.cfg.RelayRegion)
		current, codeErr := tunnel.Region(code)
		homed := a.cfg.Homed && codeErr == nil && (region == current || region == 0 && current != tunnel.CustomRegion)
		a.mu.Unlock()

		var err error
		if !homed {
			if region == 0 {
				probe, cancel := context.WithTimeout(ctx, 20*time.Second)
				region, err = tunnel.NearestRegion(probe)
				cancel()
			}
			if err == nil {
				if tunnel.Valid(code) {
					code = tunnel.CodeRegion(code, region)
				} else {
					code, err = tunnel.NewDeviceCode(region)
				}
			}
		}
		a.mu.Lock()
		if ctx.Err() != nil {
			a.mu.Unlock()
			return
		}
		if err == nil && !homed {
			old := a.cfg
			a.cfg.DeviceCode, a.cfg.Homed = code, true
			if err = a.saveConfig(); err != nil {
				a.cfg = old
			}
		}
		if err == nil {
			a.net, a.netErr = "ready", ""
			a.mu.Unlock()
			a.emit()
			a.startHost()
			return
		}
		a.net, a.netErr = "error", "配置 tailcat 中继失败："+err.Error()
		a.mu.Unlock()
		a.emit()
		select {
		case <-time.After(5 * time.Second):
		case <-ctx.Done():
			return
		}
	}
}

// SetRelay selects this host's region. The address is also used when
// connecting to other devices whose codes specify a private relay.
func (a *App) SetRelay(region int, address string) error {
	address, err := tunnel.NormalizeRelay(address)
	if err != nil {
		return err
	}
	if region != 0 && region != tunnel.CustomRegion && (region < 301 || region > 309) {
		return errors.New("无效的中继区域")
	}
	if region == tunnel.CustomRegion && address == "" {
		return errors.New("请输入自建 DERP 中继地址")
	}
	a.relayMu.Lock()
	defer a.relayMu.Unlock()
	a.mu.Lock()
	old := a.cfg
	if old.RelayRegion == region && old.RelayAddress == address {
		a.mu.Unlock()
		return nil
	}
	restart := old.RelayRegion != region || region == tunnel.CustomRegion
	a.cfg.RelayRegion, a.cfg.RelayAddress = region, address
	if restart {
		a.cfg.Homed = false
	}
	if err := a.saveConfig(); err != nil {
		a.cfg = old
		a.mu.Unlock()
		return err
	}
	if !restart {
		a.mu.Unlock()
		a.emit()
		return nil
	}
	a.cancelHoming()
	ctx, cancel := context.WithCancel(a.ctx)
	a.cancelHoming = cancel
	a.net, a.netErr, a.hostErr = "starting", "", ""
	a.mu.Unlock()
	a.stopHost()
	go a.bringUp(ctx)
	return nil
}

// startHost listens with the current codes; it replaces a running host.
func (a *App) startHost() {
	a.mu.Lock()
	if a.net != "ready" || !a.cfg.AllowControl || a.hostLn != nil || a.ctx.Err() != nil {
		a.mu.Unlock()
		return
	}
	a.hostGen++
	gen := a.hostGen
	code, pw, name, endpoint := a.cfg.DeviceCode, a.password, a.cfg.Name, a.cfg.RelayAddress
	a.mu.Unlock()

	a.mu.Lock()
	h := a.host
	a.mu.Unlock()
	var err error
	if h == nil {
		h, err = host.New(host.Config{Name: name, Code: pw, OnChange: a.emit, PairToken: a.pairToken})
	}
	var ln *tunnel.Listener
	if err == nil {
		ctx, cancel := context.WithTimeout(a.ctx, 30*time.Second)
		ln, err = tunnel.Listen(ctx, code, pw, endpoint, a.logf())
		cancel()
	}
	a.mu.Lock()
	if gen != a.hostGen { // stopped or restarted meanwhile
		a.mu.Unlock()
		if ln != nil {
			ln.Close()
		}
		return
	}
	if err != nil {
		a.hostErr = err.Error()
		a.mu.Unlock()
		a.emit()
		return
	}
	h.SetCode(pw)
	h.SetName(name)
	a.host, a.hostLn, a.hostErr = h, ln, ""
	a.setBarrierLocked()
	a.mu.Unlock()
	go h.Serve(ln)
	a.startPairServer(ln)
	go a.reconcileMembers()
	a.emit()
}

// stopHost closes the device-code listener and its sessions. The member
// listeners and their sessions are independent of the codes.
func (a *App) stopHost() {
	a.stopPairServer()
	a.mu.Lock()
	a.hostGen++
	h, ln := a.host, a.hostLn
	a.hostLn = nil
	a.mu.Unlock()
	if h != nil {
		h.KickCodeSessions()
	}
	if ln != nil {
		ln.Close()
	}
	a.emit()
}

// stopAll turns off both kinds of inbound access, ends every session and
// voids requests waiting for approval.
func (a *App) stopAll() {
	a.stopHost()
	a.stopMemberListeners()
	if n := a.memberNode(); n != nil {
		n.DisconnectAll()
	}
	a.mu.Lock()
	h := a.host
	a.host = nil
	a.mu.Unlock()
	if h != nil {
		h.Kick()
	}
	a.emit()
}

// restartHost re-keys the host after its codes or name change. Sessions
// through the old key end.
func (a *App) restartHost() {
	a.mu.Lock()
	on := a.cfg.AllowControl && a.net == "ready"
	a.mu.Unlock()
	a.stopHost()
	if on {
		a.startHost()
	}
}

// ------------------------------------------------------------- state

type StateDTO struct {
	DeviceCode    string             `json:"deviceCode"`
	Name          string             `json:"name"`
	OS            string             `json:"os"`
	Password      string             `json:"password"`
	FixedPassword bool               `json:"fixedPassword"`
	AllowControl  bool               `json:"allowControl"`
	Hosting       bool               `json:"hosting"` // listening for controllers
	CanControl    bool               `json:"canControl"`
	HostError     string             `json:"hostError"`
	Net           string             `json:"net"` // starting | ready | error
	NetError      string             `json:"netError"`
	Region        string             `json:"region"`
	RelayRegion   int                `json:"relayRegion"`
	RelayAddress  string             `json:"relayAddress"`
	Viewers       []host.SessionInfo `json:"viewers"`
	Members       MembersDTO         `json:"members"`
	Quality       int                `json:"quality"`
	FPS           int                `json:"fps"`
	Recent        []Recent           `json:"recent"`
}

var regionNames = map[int]string{tunnel.CustomRegion: "自建", 301: "纽约", 302: "旧金山", 303: "法兰克福", 304: "东京"}

// RegionName names a DERP region for people.
func RegionName(code string) string {
	r, err := tunnel.Region(code)
	if err != nil {
		return ""
	}
	return RelayName(int(r))
}

func RelayName(r int) string {
	if n, ok := regionNames[r]; ok {
		return n
	}
	return fmt.Sprintf("区域 %d", r)
}

// State returns everything the home screen shows.
func (a *App) State() StateDTO {
	m := a.membersDTO()
	a.mu.Lock()
	defer a.mu.Unlock()
	s := StateDTO{Members: m,
		Name: a.cfg.Name, OS: runtime.GOOS,
		Password: a.password, FixedPassword: a.cfg.FixedPassword != "",
		AllowControl: a.cfg.AllowControl, HostError: a.hostErr, Hosting: a.hostLn != nil,
		RelayAddress: a.cfg.RelayAddress, RelayRegion: a.cfg.RelayRegion,
		Net: a.net, NetError: a.netErr, Viewers: []host.SessionInfo{},
		Quality: a.cfg.Quality, FPS: a.cfg.FPS, Recent: append([]Recent(nil), a.cfg.Recent...),
	}
	if a.cfg.Homed {
		s.DeviceCode = a.cfg.DeviceCode
		s.Region = RegionName(a.cfg.DeviceCode)
	}
	if a.host != nil {
		s.CanControl = a.host.CanControl()
		s.Viewers = a.host.Sessions()
	}
	return s
}

// RefreshPassword generates a new random verification code.
func (a *App) RefreshPassword() string {
	p, err := a.replacePassword("")
	if err != nil {
		log.Printf("refresh verification code: %v", err)
	}
	return p
}

// SetFixedPassword sets a permanent verification code ("" = random again).
func (a *App) SetFixedPassword(p string) error {
	p = strings.TrimSpace(p)
	if p != "" && len(p) < 8 {
		return errors.New("固定验证码至少 8 位（它同时是隧道密钥的来源，越长越安全）")
	}
	_, err := a.replacePassword(p)
	return err
}

func (a *App) replacePassword(fixed string) (string, error) {
	a.mu.Lock()
	old := a.cfg.FixedPassword
	a.cfg.FixedPassword = fixed
	if err := a.saveConfig(); err != nil {
		a.cfg.FixedPassword = old
		current := a.password
		a.mu.Unlock()
		return current, err
	}
	current := fixed
	if current == "" {
		current = randomPassword()
	}
	a.password = current
	a.mu.Unlock()
	a.emit()
	a.restartHost()
	return current, nil
}

func (a *App) SetAllowControl(on bool) error {
	a.mu.Lock()
	a.cfg.AllowControl = on
	err := a.saveConfig()
	a.mu.Unlock()
	if on {
		a.startHost()
	} else {
		a.stopAll()
	}
	return err
}

func (a *App) SetName(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("设备名不能为空")
	}
	a.mu.Lock()
	a.cfg.Name = name
	err := a.saveConfig()
	a.mu.Unlock()
	a.restartHost()
	return err
}

// KickViewers disconnects everyone controlling this device, including
// "my devices" sessions and requests waiting for approval.
func (a *App) KickViewers() {
	a.mu.Lock()
	h := a.host
	a.mu.Unlock()
	if n := a.memberNode(); n != nil {
		n.DisconnectAll()
	}
	if h != nil {
		h.Kick()
	}
}

// ------------------------------------------------------------- connecting

// Target is the device a session is connected to.
type Target struct {
	Member  string // member ID when connected through "my devices"
	Code    string
	Name    string
	OS      string
	Control bool
}

// Remote is an open session and the tunnel under it.
type Remote struct {
	Target  Target
	Session *client.Session
	Tunnel  *tunnel.Conn
}

// Close ends the session and its tunnel.
func (r *Remote) Close() { r.Session.Close() }

type hostInfo struct {
	Name    string `json:"name"`
	OS      string `json:"os"`
	Control bool   `json:"control"`
}

// Connect opens a tunnel to the device with these codes and starts a
// remote session over it.
func (a *App) Connect(code, password string, cfg client.Config, h client.Handler) (*Remote, error) {
	code = tunnel.Normalize(code)
	password = strings.TrimSpace(password)
	if !tunnel.Valid(code) {
		return nil, errors.New("设备代码应为 9 位数字")
	}
	if password == "" {
		return nil, errors.New("请输入验证码")
	}
	a.mu.Lock()
	self, endpoint := a.cfg.DeviceCode, a.cfg.RelayAddress
	a.mu.Unlock()
	if code == self {
		return nil, errors.New("不能连接本机")
	}
	ctx, cancel := context.WithTimeout(a.ctx, 25*time.Second)
	defer cancel()
	r, err := Dial(ctx, code, password, endpoint, cfg, h, a.logf())
	if err != nil {
		return nil, err
	}
	a.Remember(r.Target)
	return r, nil
}

// Dial connects to the host with this device code and verification code
// through tailcat, for the desktop app (App.Connect) and the phone app.
func Dial(ctx context.Context, code, password, endpoint string, cfg client.Config, h client.Handler, logf func(string, ...any)) (*Remote, error) {
	code = tunnel.Normalize(code)
	password = strings.TrimSpace(password)
	tc, err := tunnel.Dial(ctx, code, password, endpoint, logf)
	if err != nil {
		return nil, err
	}
	var info hostInfo
	hc := &http.Client{Transport: &http.Transport{DialContext: tc.DialContext}}
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://remote-go/api/info", nil)
	if resp, err := hc.Do(req); err == nil {
		json.NewDecoder(resp.Body).Decode(&info)
		resp.Body.Close()
	}
	onClose := h.OnClose
	h.OnClose = func(err error) {
		tc.Close()
		if onClose != nil {
			onClose(err)
		}
	}
	s, err := client.Dial(ctx, tc.DialContext, fmt.Sprintf("remote-go:%d", tunnel.Port), password, encoder.CanDecodeH264(), cfg, h)
	if err != nil {
		tc.Close()
		return nil, err
	}
	t := Target{Code: code, Name: info.Name, OS: info.OS, Control: info.Control}
	if t.Name == "" {
		t.Name = code
	}
	return &Remote{Target: t, Session: s, Tunnel: tc}, nil
}

func (a *App) Remember(t Target) {
	if t.Code == "" { // "my devices" sessions are listed there instead
		return
	}
	a.mu.Lock()
	r := []Recent{{Code: t.Code, Name: t.Name, OS: t.OS, When: time.Now()}}
	for _, x := range a.cfg.Recent {
		if x.Code != t.Code && len(r) < 12 {
			r = append(r, x)
		}
	}
	a.cfg.Recent = r
	a.saveConfig()
	a.mu.Unlock()
	a.emit()
}

// ForgetRecent removes a device from the recent list.
func (a *App) ForgetRecent(code string) {
	a.mu.Lock()
	var r []Recent
	for _, x := range a.cfg.Recent {
		if x.Code != code {
			r = append(r, x)
		}
	}
	a.cfg.Recent = r
	a.saveConfig()
	a.mu.Unlock()
	a.emit()
}

// SetDefaults stores the default stream quality and frame rate.
func (a *App) SetDefaults(quality, fps int) {
	a.mu.Lock()
	a.cfg.Quality, a.cfg.FPS = quality, fps
	a.saveConfig()
	a.mu.Unlock()
	a.emit()
}

// ------------------------------------------------------------- helpers

func randomPassword() string {
	const alphabet = "abcdefghjkmnpqrstuvwxyz23456789" // no 0/o/1/l/i
	b := make([]byte, 8)
	for i := range b {
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		b[i] = alphabet[n.Int64()]
	}
	return string(b)
}
