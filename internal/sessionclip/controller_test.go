package sessionclip

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/nsgo-dev/nsgo/core"

	"github.com/i5shuyi/remote-go/internal/client"
	"github.com/i5shuyi/remote-go/internal/clipshare"
)

type fileBoard struct {
	version atomic.Uint64
	item    clipshare.Content
}

func (b *fileBoard) Version() uint64                                     { return b.version.Load() }
func (b *fileBoard) Read() ([]byte, bool)                                { return nil, true }
func (b *fileBoard) Write([]byte) bool                                   { return false }
func (b *fileBoard) ReadRich(context.Context) (clipshare.Content, error) { return b.item, nil }
func (b *fileBoard) WriteRich(c clipshare.Content) error                 { b.item = c; b.version.Add(1); return nil }

// Copy and immediately paste, before the first background poll. The V key
// on the real WebSocket may arrive only after the complete file is readable.
func TestImmediatePasteWaitsForRemoteClipboard(t *testing.T) {
	t.Setenv("NSGO_CACHE_DIR", t.TempDir())
	src := filepath.Join(t.TempDir(), "共享文件")
	payload := bytes.Repeat([]byte{0x42}, 2*clipshare.ChunkSize+17)
	if err := os.WriteFile(src, payload, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pasted := make(chan bool, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer ws.CloseNow()
		ws.SetReadLimit(clipshare.ChunkSize * 2)
		var board fileBoard
		var host clipshare.Host
		defer host.Close()
		for {
			_, packet, err := ws.Read(ctx)
			if err != nil {
				return
			}
			var m struct {
				T, C string
				clipshare.Message
			}
			if json.Unmarshal(packet, &m) != nil {
				return
			}
			switch m.T {
			case "clip":
				if reply := host.HandleContext(ctx, &board, m.Message); reply != nil {
					b, err := json.Marshal(reply.Packet())
					if err != nil || ws.Write(ctx, websocket.MessageText, b) != nil {
						return
					}
				}
			case "kd":
				if m.C == "KeyV" {
					var content []byte
					if len(board.item.Files) == 1 {
						content, _ = os.ReadFile(board.item.Files[0])
					}
					pasted <- bytes.Equal(content, payload)
				}
			}
		}
	}))
	defer server.Close()
	posted := make(chan func(), 64)
	cx := core.NewRoot(core.RootOptions{Post: func(fn func()) { posted <- fn }})
	var session *client.Session
	controller := New(cx, func() *client.Session { return session }, func(err error) { t.Error(err) })
	board := &fileBoard{item: clipshare.Content{Files: []string{src}}}
	controller.board = board
	var dial net.Dialer
	var err error
	session, err = client.Dial(ctx, dial.DialContext, strings.TrimPrefix(server.URL, "http://"), "test", false, client.Config{}, client.Handler{OnClipboard: func(m clipshare.Message) { cx.Post(func() { controller.Receive(m) }) }})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	controller.Rich = true
	controller.SetAvailable(true)
	controller.Set(true)
	defer controller.Set(false)
	board.version.Add(1)
	session.SendKeys("ControlLeft", "KeyV")
	for {
		select {
		case fn := <-posted:
			fn()
		case ok := <-pasted:
			if !ok {
				t.Fatal("paste arrived before file publication")
			}
			return
		case <-ctx.Done():
			t.Fatal("paste did not arrive")
		}
	}
}
