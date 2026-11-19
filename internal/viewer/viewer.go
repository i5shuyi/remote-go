// Package viewer is the command-line controlling side: it exposes a remote
// host, reached through a tailcat tunnel, on 127.0.0.1 so any local browser
// can be used as the remote-desktop UI.
package viewer

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// DialFunc dials the remote host (e.g. tunnel.Conn.DialContext).
type DialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// Proxy exposes a remote host on 127.0.0.1 under a random path prefix, so a
// local web view (or browser) can talk to it over the tailnet. The prefix
// keeps other local web pages from using the proxy.
type Proxy struct {
	URL    string // http://127.0.0.1:port/<token>/
	Remote string
	ln     net.Listener
	hs     *http.Server
}

func NewProxy(dial DialFunc, target string, port int, listen string) (*Proxy, error) {
	remote := net.JoinHostPort(target, fmt.Sprint(port))
	rp := httputil.NewSingleHostReverseProxy(&url.URL{Scheme: "http", Host: remote})
	rp.Transport = &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dial(ctx, "tcp", remote)
		},
		ResponseHeaderTimeout: 30 * time.Second,
	}
	rp.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		log.Printf("无法连接到 %s: %v", remote, err)
		http.Error(w, "无法连接到远程设备 "+remote, http.StatusBadGateway)
	}
	tok := make([]byte, 16)
	rand.Read(tok)
	prefix := "/" + hex.EncodeToString(tok)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, prefix+"/") {
			http.NotFound(w, r)
			return
		}
		r.URL.Path = strings.TrimPrefix(r.URL.Path, prefix)
		r.URL.RawPath = ""
		// The web view's origin (wails://, http://127.0.0.1) never matches the
		// host; the token above is what authorises local callers.
		r.Header.Del("Origin")
		rp.ServeHTTP(w, r)
	})
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return nil, err
	}
	p := &Proxy{URL: "http://" + ln.Addr().String() + prefix + "/", Remote: remote, ln: ln,
		hs: &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}}
	go p.hs.Serve(ln)
	return p, nil
}

// WSURL is the proxied WebSocket endpoint of the host.
func (p *Proxy) WSURL() string { return "ws" + strings.TrimPrefix(p.URL, "http") + "ws" }

func (p *Proxy) Close() error { return p.hs.Close() }

// Run proxies the remote host to a local URL and opens it in the browser.
func Run(ctx context.Context, dial DialFunc, target string, port int, listen string, open bool) error {
	p, err := NewProxy(dial, target, port, listen)
	if err != nil {
		return err
	}
	defer p.Close()
	log.Printf("控制端已就绪：%s", p.URL)
	if open {
		openBrowser(p.URL)
	}
	<-ctx.Done()
	return nil
}

func openBrowser(u string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", u)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	default:
		cmd = exec.Command("xdg-open", u)
	}
	_ = cmd.Start()
}
