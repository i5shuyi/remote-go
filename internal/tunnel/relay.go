package tunnel

import (
	"errors"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"

	"tailscale.com/tailcfg"
	"tailscale.com/util/dnsname"
)

// CustomRegion is encoded by the previously unused final digit 0.
const CustomRegion = 900

// NormalizeRelay accepts a DERP hostname, host:port or HTTPS endpoint.
func NormalizeRelay(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	if ip, err := netip.ParseAddr(raw); err == nil && ip.Is6() {
		raw = "[" + raw + "]"
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" ||
		(u.EscapedPath() != "" && u.EscapedPath() != "/" && u.EscapedPath() != "/derp") || strings.HasSuffix(u.Host, ":") {
		return "", errors.New("请输入 DERP 域名或 HTTPS 地址，可指定端口")
	}
	host := strings.ToLower(u.Hostname())
	if host == "" || strings.HasPrefix(u.Host, "[") && !strings.Contains(host, ":") {
		return "", errors.New("DERP 主机名无效")
	}
	if ip, err := netip.ParseAddr(host); err == nil && ip.Zone() == "" {
		host = ip.String()
	} else if dnsname.ValidHostname(host) != nil {
		return "", errors.New("DERP 主机名无效")
	}
	port := 443
	if u.Port() != "" {
		port, err = strconv.Atoi(u.Port())
		if err != nil || port < 1 || port > 65535 {
			return "", errors.New("DERP 端口应为 1 至 65535")
		}
	}
	return net.JoinHostPort(host, strconv.Itoa(port)), nil
}

// relayForCode is shared by the listener and address derivation. Public
// codes always select their original region, even with a private relay saved.
func relayForCode(code, endpoint string) (tailcfg.DERPRegionID, *tailcfg.DERPRegion, error) {
	id, err := Region(code)
	if err != nil || id != CustomRegion {
		return id, nil, err
	}
	endpoint, err = NormalizeRelay(endpoint)
	if err != nil {
		return 0, nil, err
	}
	if endpoint == "" {
		return 0, nil, errors.New("设备代码末位为 0，请配置与对方相同的自建 DERP 中继地址")
	}
	return 0, customRelay(endpoint), nil
}

// customRelay describes a normalized self-hosted endpoint as a tailcat region.
func customRelay(endpoint string) *tailcfg.DERPRegion {
	host, port, _ := net.SplitHostPort(endpoint)
	p, _ := strconv.Atoi(port)
	// tailcat assigns ID 1 to the first embedded region when decoding an address.
	return &tailcfg.DERPRegion{RegionID: 1, RegionCode: "custom", RegionName: endpoint,
		Nodes: []*tailcfg.DERPNode{{RegionID: 1, Name: "1a", HostName: host, DERPPort: p}}}
}
