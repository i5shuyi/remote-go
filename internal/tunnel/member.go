package tunnel

import (
	"context"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/tailscale/tailcat"
	"go4.org/mem"
	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
)

// PairPort is the pairing endpoint inside an authenticated device-code
// tunnel. It is separate from the desktop protocol on Port.
const PairPort = 7901

// MemberPort is where a member node serves mutual-TLS HTTP inside its own
// tailcat network.
const MemberPort = 7900

// MemberRelay names the relay a member listener is homed on: a public
// tailcat region, or a normalized self-hosted endpoint. Exactly one is set.
type MemberRelay struct {
	Region  int    `json:"region,omitempty"`
	Address string `json:"address,omitempty"`
}

// Validate checks that the descriptor is in the form every member derives
// the same address from.
func (r MemberRelay) Validate() error {
	switch {
	case r.Region != 0 && r.Address != "":
		return errors.New("中继描述只能指定区域或地址之一")
	case r.Address != "":
		n, err := NormalizeRelay(r.Address)
		if err != nil {
			return err
		}
		if n != r.Address {
			return errors.New("自建中继地址未规范化")
		}
		return nil
	case r.Region > regionBase && r.Region <= regionBase+9:
		return nil
	default:
		return fmt.Errorf("公共中继区域 %d 无效", r.Region)
	}
}

// Canonical returns the RFC 8785 bytes fed into address derivation. Both
// shapes contain only an integer or a normalized host:port, so the plain
// encoding is already canonical.
func (r MemberRelay) Canonical() ([]byte, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	if r.Address != "" {
		a, _ := json.Marshal(r.Address)
		return append(append([]byte(`{"address":`), a...), '}'), nil
	}
	return fmt.Appendf(nil, `{"region":%d}`, r.Region), nil
}

func (r MemberRelay) region() (tailcfg.DERPRegionID, *tailcfg.DERPRegion) {
	if r.Address != "" {
		return 0, customRelay(r.Address)
	}
	return tailcfg.DERPRegionID(r.Region), nil
}

// MemberEndpoint is everything that determines a member's tailcat address
// for one key generation on one relay.
type MemberEndpoint struct {
	GroupKey   []byte // K, 32 bytes
	GroupID    []byte // 32 bytes
	KeyVersion []byte // 32 bytes
	MemberID   []byte // 32 bytes
	Relay      MemberRelay
}

func (e MemberEndpoint) keys() (key.NodePrivate, tailcat.PresharedKey, error) {
	if len(e.GroupKey) != 32 || len(e.GroupID) != 32 || len(e.KeyVersion) != 32 || len(e.MemberID) != 32 {
		return key.NodePrivate{}, tailcat.PresharedKey{}, errors.New("成员地址输入长度无效")
	}
	relay, err := e.Relay.Canonical()
	if err != nil {
		return key.NodePrivate{}, tailcat.PresharedKey{}, err
	}
	info := make([]byte, 0, 96+len(relay))
	info = append(append(append(append(info, e.GroupID...), e.KeyVersion...), e.MemberID...), relay...)
	seed, err := hkdf.Key(sha256.New, e.GroupKey, []byte("remote-go/member-address/v1"), string(info), 64)
	if err != nil {
		return key.NodePrivate{}, tailcat.PresharedKey{}, err
	}
	nk := key.NodePrivateFromRaw32(mem.B(seed[:32]))
	var psk tailcat.PresharedKey
	copy(psk[:], seed[32:])
	return nk, psk, nil
}

// Addr returns the member's tailcat address.
func (e MemberEndpoint) Addr() (tailcat.Addr, error) {
	nk, psk, err := e.keys()
	if err != nil {
		return "", err
	}
	region, custom := e.Relay.region()
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

// ListenMember starts this member's tailcat server for one endpoint and
// listens on MemberPort inside it.
func ListenMember(ctx context.Context, e MemberEndpoint, logf func(string, ...any)) (*Listener, error) {
	nk, psk, err := e.keys()
	if err != nil {
		return nil, err
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	region, custom := e.Relay.region()
	srv := &tailcat.Server{Key: nk, PresharedKey: psk, RegionID: region, Region: custom, Logf: logf}
	ln, err := srv.Listen(ctx, "tcp", fmt.Sprintf(":%d", MemberPort))
	if err != nil {
		srv.Close()
		return nil, fmt.Errorf("启动成员监听失败：%w", err)
	}
	return &Listener{Listener: ln, srv: srv, addr: srv.TailcatAddr()}, nil
}

// DialMember opens a tunnel to a member endpoint. Reaching it proves only
// that the transport answers: identity is established by the inner TLS.
func DialMember(ctx context.Context, e MemberEndpoint, logf func(string, ...any)) (*Conn, error) {
	addr, err := e.Addr()
	if err != nil {
		return nil, err
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	c := &tailcat.Client{Server: addr, Logf: logf}
	if _, err := c.Ping(ctx); err != nil {
		c.Close()
		logf("member dial %s: %v", addr, err)
		return nil, errors.New("无法联系设备：它可能已离线、未开启直连，或已将本机移出设备组")
	}
	return &Conn{c: c}, nil
}
