package gui

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/i5shuyi/remote-go/internal/backend"
	"github.com/i5shuyi/remote-go/internal/client"
	"github.com/i5shuyi/remote-go/internal/screen"
)

// devSession opens a session window straight to a host on a plain TCP
// address, bypassing tailcat, for development:
//
//	RG_DEV_SESSION=127.0.0.1:7900/123456 remote-go
func (g *gui) devSession() {
	if dir := os.Getenv("RG_DEV_SNAPSHOT"); dir != "" {
		// RG_DEV_SNAPSHOT=dir writes the windows as PNGs after a few seconds.
		// RG_DEV_SNAPSHOT_AFTER=8,25 takes one set per delay (seconds) into
		// dir/1, dir/2, …
		after := strings.Split(os.Getenv("RG_DEV_SNAPSHOT_AFTER"), ",")
		if after[0] == "" {
			after = []string{"6"}
		}
		go func() {
			start := time.Now()
			for i, a := range after {
				d, _ := time.ParseDuration(strings.TrimSpace(a) + "s")
				time.Sleep(time.Until(start.Add(d)))
				out := dir
				if len(after) > 1 {
					out = filepath.Join(dir, fmt.Sprint(i+1))
					os.MkdirAll(out, 0o755)
				}
				screen.Snapshot(out)
			}
		}()
	}
	if spec := os.Getenv("RG_DEV_CONNECT"); spec != "" {
		// RG_DEV_CONNECT=<device code>/<verification code>: connect through
		// tailcat exactly as the password dialog does.
		code, pw, _ := strings.Cut(spec, "/")
		g.openSession(code, pw, func(err error) {
			if err != nil {
				log.Printf("RG_DEV_CONNECT: %v", err)
			}
		})
	}
	spec := os.Getenv("RG_DEV_SESSION")
	if spec == "" {
		return
	}
	addr, code, _ := strings.Cut(spec, "/")
	if g.panel != nil {
		g.async(func() error { return g.panel.Open(addr, code, addr) }, func(err error) {
			if err != nil {
				log.Print(err)
			}
		})
		return
	}
	g.openSessionWith(func(cfg client.Config, h client.Handler) (*backend.Remote, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		var d net.Dialer
		s, err := client.Dial(ctx, d.DialContext, addr, code, true, cfg, h)
		if err != nil {
			return nil, err
		}
		return &backend.Remote{Target: backend.Target{Code: addr, Name: "开发会话", Control: true}, Session: s}, nil
	}, func(err error) {
		if err != nil {
			log.Printf("RG_DEV_SESSION: %v", err)
		}
	})
}
