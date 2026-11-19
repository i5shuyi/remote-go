package host

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/i5shuyi/remote-go/internal/clipshare"
	"github.com/i5shuyi/remote-go/internal/input"
)

// Exercises the host's WebSocket messages without touching a system clipboard.
type memoryBoard struct {
	data     []byte
	revision uint64
}

func (b *memoryBoard) Version() uint64        { return b.revision }
func (b *memoryBoard) Read() ([]byte, bool)   { return b.data, true }
func (b *memoryBoard) Write(data []byte) bool { b.data = data; b.revision++; return true }

type connectedInput struct{ input.Injector }

func TestClipboardWebSocket(t *testing.T) {
	for _, viewOnly := range []bool{false, true} {
		name := "control"
		if viewOnly {
			name = "view-only"
		}
		t.Run(name, func(t *testing.T) {
			board := &memoryBoard{}
			s := &Server{cfg: Config{ViewOnly: viewOnly}, clipboard: board}
			if !viewOnly {
				s.inj = connectedInput{}
			}
			var handlers sync.WaitGroup
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				handlers.Add(1)
				defer handlers.Done()
				c, err := websocket.Accept(w, r, nil)
				if err != nil {
					return
				}
				defer c.CloseNow()
				ss := session{s: s, c: c}
				for {
					_, b, err := c.Read(r.Context())
					if err != nil {
						return
					}
					var m inMsg
					if json.Unmarshal(b, &m) == nil {
						ss.handle(r.Context(), m)
					}
				}
			}))
			defer ts.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			connect := func() *websocket.Conn {
				c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http"), nil)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { c.CloseNow() })
				return c
			}
			c := connect()
			send := func(gen uint64, op, text string) {
				b, _ := json.Marshal(clipshare.Message{Generation: gen, Operation: op, Data: []byte(text)}.Packet())
				if err := c.Write(ctx, websocket.MessageText, b); err != nil {
					t.Fatal(err)
				}
			}
			read := func(want string) {
				_, b, err := c.Read(ctx)
				var m struct {
					T string `json:"t"`
					clipshare.Message
				}
				if err != nil || json.Unmarshal(b, &m) != nil || m.T != want || want == "clip" && m.Operation != "result" {
					t.Fatalf("expected %s, got %s (%v)", want, b, err)
				}
			}
			barrier := func() {
				if err := c.Write(ctx, websocket.MessageText, []byte(`{"t":"ping"}`)); err != nil {
					t.Fatal(err)
				}
				read("pong") // earlier ignored polls must have produced no reply
			}
			send(1, "poll", "before")
			barrier()
			send(1, "on", "")
			send(1, "poll", "共享文本")
			if !viewOnly {
				read("clip")
			}
			barrier()
			send(1, "off", "")
			send(1, "poll", "after")
			barrier()
			c.CloseNow()
			c = connect()
			send(1, "poll", "old token")
			barrier()
			c.CloseNow()
			// No further handlers can write; read the final value after shutdown.
			ts.Close()
			handlers.Wait()
			want := "共享文本"
			if viewOnly {
				want = ""
			}
			if string(board.data) != want {
				t.Fatalf("final clipboard %q, want %q", board.data, want)
			}
		})
	}
}
