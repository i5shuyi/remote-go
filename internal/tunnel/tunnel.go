// Package tunnel connects devices with tailcat (github.com/tailscale/tailcat):
// Tailscale's data plane — WireGuard, NAT traversal, DERP relays — without
// its control plane, accounts or login.
//
// UU-Remote-style codes replace tailcat's long addresses. A host's tailcat
// address is fully determined by its WireGuard node key, pre-shared key and
// DERP region, so both sides derive it from what the user types:
//
//	device code   = 8 random digits + 1 digit naming the DERP region
//	verification  = the host's current password
//	Argon2id(verification, salt = device code) → node key ‖ pre-shared key
//
// The host listens with that key; the controller computes the same address
// and dials it. No rendezvous server exists: the DERP relay only forwards
// packets to the public key it is asked for, and without both codes nobody
// can compute the key, let alone the pre-shared key the handshake needs.
package tunnel

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/tailscale/tailcat"
	"go4.org/mem"
	"golang.org/x/crypto/argon2"
	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
)

// Port is the remote desktop's TCP port inside the tunnel.
const Port = 7900

// regionBase maps the code's last digit to a tailcat DERP region:
// 1 → 301 (New York), 2 → 302 (San Francisco), 3 → 303 (Frankfurt),
// 4 → 304 (Tokyo), …
const regionBase = 300

var codeRE = regexp.MustCompile(`^[1-9][0-9]{8}$`)

// Normalize strips spaces and dashes from a typed device code.
func Normalize(code string) string {
	return strings.NewReplacer(" ", "", "-", "", " ", "").Replace(strings.TrimSpace(code))
}

// Valid reports whether code is a well-formed device code.
func Valid(code string) bool { return codeRE.MatchString(Normalize(code)) }

// Region returns the DERP region a device code names.
func Region(code string) (tailcfg.DERPRegionID, error) {
	code = Normalize(code)
	if !codeRE.MatchString(code) {
		return 0, errors.New("设备代码应为 9 位数字")
	}
	if code[8] == '0' {
		return CustomRegion, nil
	}
	return tailcfg.DERPRegionID(regionBase + int(code[8]-'0')), nil
}

func regionDigit(region tailcfg.DERPRegionID) (int, bool) {
	if region == CustomRegion {
		return 0, true
	}
	d := int(region) - regionBase
	return d, d >= 1 && d <= 9
}

// NewDeviceCode returns a fresh device code homed in region.
func NewDeviceCode(region tailcfg.DERPRegionID) (string, error) {
	d, ok := regionDigit(region)
	if !ok {
		return "", fmt.Errorf("DERP 区域 %d 无法编入设备代码", region)
	}
	n, _ := rand.Int(rand.Reader, big.NewInt(90_000_000))
	return fmt.Sprintf("%d%d", 10_000_000+n.Int64(), d), nil
}

// CodeRegion re-homes an existing code to region, keeping its digits.
func CodeRegion(code string, region tailcfg.DERPRegionID) string {
	code = Normalize(code)
	d, ok := regionDigit(region)
	if !Valid(code) || !ok {
		return code
	}
	return code[:8] + fmt.Sprint(d)
}

// keys derives the host's node key and pre-shared key. Argon2id makes every
// guess of the verification code cost ~64 MiB and tens of milliseconds.
func keys(code, password string) (key.NodePrivate, tailcat.PresharedKey) {
	seed := argon2.IDKey([]byte(password), []byte("remote-go/tailcat/v1|"+Normalize(code)), 3, 64<<10, 4, 64)
	nk := key.NodePrivateFromRaw32(mem.B(seed[:32]))
	var psk tailcat.PresharedKey
	copy(psk[:], seed[32:])
	return nk, psk
}

var keyCache struct {
	sync.Mutex
	in  [2]string
	nk  key.NodePrivate
	psk tailcat.PresharedKey
}

func cachedKeys(code, password string) (key.NodePrivate, tailcat.PresharedKey) {
	keyCache.Lock()
	defer keyCache.Unlock()
	if keyCache.in != [2]string{code, password} {
		keyCache.nk, keyCache.psk = keys(code, password)
		keyCache.in = [2]string{code, password}
	}
	return keyCache.nk, keyCache.psk
}

// Addr returns the tailcat address of the host with this device code and
// verification code.
func Addr(code, password, endpoint string) (tailcat.Addr, error) {
	region, custom, err := relayForCode(code, endpoint)
	if err != nil {
		return "", err
	}
	nk, psk := cachedKeys(Normalize(code), password)
	ci := tailcat.ConnInfo{
		ServerPublic:      tailcat.NodePublic{NodePublic: nk.Public()},
		ServerDiscoPublic: tailcat.DiscoPublicForNode(nk),
		PresharedKey:      psk,
		RegionID:          region,
	}
	if custom != nil {
		ci.Region = []*tailcfg.DERPRegion{custom}
	}
	return ci.Addr(), nil
}

// NearestRegion measures the tailcat DERP regions and returns the closest.
func NearestRegion(ctx context.Context) (tailcfg.DERPRegionID, error) {
	dm, err := tailcat.FetchDERPMap(ctx)
	if err != nil {
		return 0, fmt.Errorf("获取中继列表失败：%w", err)
	}
	eligible := &tailcfg.DERPMap{Regions: make(map[tailcfg.DERPRegionID]*tailcfg.DERPRegion)}
	for id, r := range dm.Regions {
		if id > regionBase && id <= regionBase+9 && r != nil && len(r.Nodes) > 0 {
			eligible.Regions[id] = r
		}
	}
	if len(eligible.Regions) == 0 {
		return 0, errors.New("中继列表中没有可用于设备代码的公共区域")
	}
	id, err := tailcat.PickBestRegion(ctx, eligible)
	if err != nil {
		return 0, err
	}
	if id == 0 {
		for rid := range eligible.Regions { // nothing answered: any region will do
			if id == 0 || rid < id {
				id = rid
			}
		}
	}
	return id, nil
}

// Listener is a host's endpoint: a tailcat server keyed by its codes.
type Listener struct {
	net.Listener
	srv  *tailcat.Server
	addr tailcat.Addr
}

// Listen starts a tailcat server for the device code and verification code
// and listens on Port inside it.
func Listen(ctx context.Context, code, password, endpoint string, logf func(string, ...any)) (*Listener, error) {
	region, custom, err := relayForCode(code, endpoint)
	if err != nil {
		return nil, err
	}
	nk, psk := cachedKeys(Normalize(code), password)
	if logf == nil {
		logf = func(string, ...any) {}
	}
	srv := &tailcat.Server{Key: nk, PresharedKey: psk, RegionID: region, Region: custom, Logf: logf}
	ln, err := srv.Listen(ctx, "tcp", fmt.Sprintf(":%d", Port))
	if err != nil {
		srv.Close()
		return nil, fmt.Errorf("启动 tailcat 失败：%w", err)
	}
	return &Listener{Listener: ln, srv: srv, addr: srv.TailcatAddr()}, nil
}

// TailcatAddr is the full tailcat address, for the tailcat CLI.
func (l *Listener) TailcatAddr() tailcat.Addr { return l.addr }

// Close stops the server, ending every connection through it.
func (l *Listener) Close() error {
	l.Listener.Close()
	return l.srv.Close()
}

// Conn is a controller's tunnel to one host.
type Conn struct {
	c *tailcat.Client
}

// Dial opens a tunnel to the host with these codes. It returns once the
// host answers (through DERP first; magicsock upgrades to a direct UDP path
// when NAT traversal succeeds), or with an error when nobody does — wrong
// codes look exactly like an offline host.
func Dial(ctx context.Context, code, password, endpoint string, logf func(string, ...any)) (*Conn, error) {
	addr, err := Addr(code, password, endpoint)
	if err != nil {
		return nil, err
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	c := &tailcat.Client{Server: addr, Logf: logf}
	if _, err := c.Ping(ctx); err != nil {
		c.Close()
		if ctx.Err() != nil {
			return nil, ErrUnreachable
		}
		return nil, fmt.Errorf("%w（%v）", ErrUnreachable, err)
	}
	return &Conn{c: c}, nil
}

// ErrUnreachable means no host answered for the device code and
// verification code: it is offline, or one of the codes is wrong.
var ErrUnreachable = errors.New("连接失败：对方不在线，或设备代码 / 验证码不正确")

// DialContext dials the host's remote-desktop port; addr is ignored.
func (c *Conn) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	return c.c.DialTCPPort(ctx, Port)
}

// DialPort dials another TCP port inside the tunnel, such as PairPort.
func (c *Conn) DialPort(ctx context.Context, port uint16) (net.Conn, error) {
	return c.c.DialTCPPort(ctx, port)
}

// ListenPort listens on another TCP port of the same tailcat server.
func (l *Listener) ListenPort(ctx context.Context, port uint16) (net.Listener, error) {
	return l.srv.Listen(ctx, "tcp", fmt.Sprintf(":%d", port))
}

// Path reports how packets travel: "direct" (P2P UDP) or "relay" (DERP).
func (c *Conn) Path(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	r, err := c.c.DiscoPing(ctx)
	if err != nil || r == nil {
		return ""
	}
	if r.Endpoint != "" {
		return "direct"
	}
	if r.DERPRegionCode != "" {
		return "relay " + r.DERPRegionCode
	}
	return ""
}

// Close tears the tunnel down.
func (c *Conn) Close() error { return c.c.Close() }

// Logf returns a logger for tailcat's diagnostics when verbose, else nil.
func Logf(verbose bool) func(string, ...any) {
	if verbose {
		return log.Printf
	}
	return nil
}

var _ io.Closer = (*Conn)(nil)
