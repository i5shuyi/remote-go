package viewer

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/i5shuyi/remote-go/internal/host"
)

func TestProxy(t *testing.T) {
	h, err := host.New(host.Config{Name: "test-pc", Code: "123456", ViewOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(h.Handler())
	defer ts.Close()
	hostAddr := strings.TrimPrefix(ts.URL, "http://")
	hostName, port, _ := net.SplitHostPort(hostAddr)
	var d net.Dialer
	dial := func(ctx context.Context, n, _ string) (net.Conn, error) { return d.DialContext(ctx, n, hostAddr) }

	p, err := NewProxy(dial, hostName, mustAtoi(port), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	get := func(u string) (int, string) {
		resp, err := http.Get(u)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := get(p.URL + "api/info"); code != 200 || !strings.Contains(body, `"test-pc"`) {
		t.Fatalf("info via proxy: %d %s", code, body)
	}
	if code, body := get(p.URL + "viewer.js"); code != 200 || !strings.Contains(body, "RemoteSession") {
		t.Fatalf("viewer.js via proxy: %d", code)
	}
	if code, _ := get(strings.Replace(p.URL, p.URL[strings.LastIndex(p.URL[:len(p.URL)-1], "/"):], "/wrongtoken/", 1) + "api/info"); code != 404 {
		t.Fatalf("wrong token should 404, got %d", code)
	}
	if code, _ := get(p.URL + "app/index.html"); code != 404 {
		t.Fatalf("app assets must not be served by host, got %d", code)
	}

	// WebSocket through the proxy, with a foreign Origin like a web view sends.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	hdr := http.Header{"Origin": []string{"wails://wails"}}
	c, _, err := websocket.Dial(ctx, p.WSURL()+"?code=123456", &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	_, data, err := c.Read(ctx)
	if err != nil || !strings.Contains(string(data), `"info"`) {
		t.Fatalf("ws info: %v %s", err, data)
	}
}

func mustAtoi(s string) int {
	n := 0
	for _, c := range s {
		n = n*10 + int(c-'0')
	}
	return n
}
