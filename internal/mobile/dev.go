package mobile

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/i5shuyi/remote-go/internal/screen"
)

// devConnect helps develop the phone app on a desktop:
//
//	RG_DEV_CONNECT=<device code>/<verification code>  connects at start
//	RG_DEV_SNAPSHOT=<dir> RG_DEV_SNAPSHOT_AFTER=8,20  saves the window as PNGs (macOS)
func (a *app) devConnect() {
	if spec := os.Getenv("RG_DEV_CONNECT"); spec != "" {
		code, pw, _ := strings.Cut(spec, "/")
		a.open(code, pw, func(err error) {
			if err != nil {
				log.Printf("RG_DEV_CONNECT: %v", err)
				return
			}
			// RG_DEV_OPEN=panel or keyboard opens that part, for snapshots.
			if r := a.session.GetUntracked(); r != nil {
				switch os.Getenv("RG_DEV_OPEN") {
				case "panel":
					r.panel.Set(true)
				case "keyboard":
					r.kb.Set(true)
				}
			}
		})
	}
	if dir := os.Getenv("RG_DEV_SNAPSHOT"); dir != "" {
		after := strings.Split(os.Getenv("RG_DEV_SNAPSHOT_AFTER"), ",")
		if after[0] == "" {
			after = []string{"6"}
		}
		go func() {
			start := time.Now()
			for i, s := range after {
				d, _ := time.ParseDuration(strings.TrimSpace(s) + "s")
				time.Sleep(time.Until(start.Add(d)))
				out := filepath.Join(dir, fmt.Sprint(i+1))
				os.MkdirAll(out, 0o755)
				screen.Snapshot(out)
			}
		}()
	}
}
