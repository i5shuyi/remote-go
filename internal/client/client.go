// Package client is the controlling side of a remote session: a WebSocket
// to a host's /ws (normally dialed through the embedded Tailscale node),
// receiving video and sending mouse/keyboard input. It is the Go
// counterpart of webui/static/viewer.js, used by the native desktop app.
package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/i5shuyi/remote-go/internal/clipshare"
)

// Frame kinds: the first byte of each binary message.
const (
	FrameJPEG     = 1
	FrameH264Key  = 2
	FrameH264Diff = 3
)

// ErrBadCode is returned when the host rejects the verification code.
var ErrBadCode = errors.New("验证码错误")

// ErrTimeout ends a session whose host stopped answering: it quit, lost
// its network, or its tunnel went away without closing the connection.
var ErrTimeout = errors.New("对方已无响应")

// Timeout is how long a session waits without hearing from the host (it
// answers the client's ping every second) before giving up.
var Timeout = 6 * time.Second

// Info is the host's greeting.
type Info struct {
	ClipboardRich bool   `json:"clipboardRich"`
	Clipboard     bool   `json:"clipboard"`
	Name          string `json:"name"`
	OS            string `json:"os"`
	Control       bool   `json:"control"`
	W             int    `json:"w"`
	H             int    `json:"h"`
}

// Stats is reported about once a second.
type Stats struct {
	Codec   string  // "H.264" or "JPEG"
	FPS     int     // frames received in the last second
	Mbps    float64 // received bitrate
	RTT     time.Duration
	Control bool
	Error   string // last error the host reported
}

// Handler receives a session's events. Every callback is optional and may
// run on any goroutine; OnFrame runs on the reader goroutine, in order.
type Handler struct {
	OnClipboard func(clipshare.Message)
	OnInfo      func(Info)
	OnVideo     func(codec string, w, h int) // the stream (re)starts; decoders reset
	OnFrame     func(kind byte, data []byte)
	OnStats     func(Stats)
	OnClose     func(err error)
}

// Config is the stream the viewer asks for.
type Config struct {
	Quality int `json:"q"`   // JPEG quality / H.264 bitrate scale, 1–100
	FPS     int `json:"fps"` // 5–60
	Width   int `json:"w"`   // maximum width; the host scales down to it
}

// DialFunc dials the host's TCP address, e.g. tsnet.Server.Dial.
type DialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// Session is one open connection to a host.
type Session struct {
	c *websocket.Conn
	h Handler

	ctx    context.Context
	cancel context.CancelFunc
	once   sync.Once

	mu      sync.Mutex
	pending *msg // latest coalesced mouse move
	codec   string
	control bool
	lastErr string
	rtt     time.Duration

	pasteRequests chan uint64
	pasteSeq      uint64
	pasteMod      string
	keyMu         sync.Mutex // orders physical keys, menu chords and focus-loss releases
	down          map[string]bool

	frames atomic.Int64
	bytes  atomic.Int64
	seen   atomic.Int64 // UnixNano of the last message from the host
}

type msg struct {
	T  string  `json:"t"`
	X  float64 `json:"x"`
	Y  float64 `json:"y"`
	B  int     `json:"b"`
	DX float64 `json:"dx,omitempty"`
	DY float64 `json:"dy,omitempty"`
	C  string  `json:"c,omitempty"`
}

// Dial connects to the host at addr ("host:port") with the verification
// code. h264 asks for H.264 when the caller can decode it.
func Dial(ctx context.Context, dial DialFunc, addr, code string, h264 bool, cfg Config, h Handler) (*Session, error) {
	q := url.Values{"code": {strings.TrimSpace(code)}}
	if h264 {
		q.Set("codec", "h264")
	}
	hc := &http.Client{Transport: &http.Transport{DialContext: dial, DisableKeepAlives: true}}
	c, resp, err := websocket.Dial(ctx, "ws://"+addr+"/ws?"+q.Encode(), &websocket.DialOptions{
		HTTPClient: hc, CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		if resp != nil {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
			resp.Body.Close()
			msg := strings.TrimSpace(string(body))
			switch resp.StatusCode {
			case http.StatusUnauthorized:
				if strings.Contains(msg, "稍后") { // locked out after repeated failures
					return nil, fmt.Errorf("%w：%s", ErrBadCode, msg)
				}
				return nil, ErrBadCode
			case http.StatusForbidden:
				return nil, errors.New("对方设置了访问白名单，你的账号不在其中")
			}
			if msg != "" {
				return nil, fmt.Errorf("连接被拒绝（HTTP %d）：%s", resp.StatusCode, msg)
			}
		}
		return nil, err
	}
	c.SetReadLimit(32 << 20)
	s := &Session{c: c, h: h, down: map[string]bool{}}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	s.SetConfig(cfg)
	s.seen.Store(time.Now().UnixNano())
	go s.read()
	go s.tick()
	return s, nil
}

// SetConfig changes the stream's quality, frame rate and size.
func (s *Session) SetConfig(c Config) {
	s.sendJSON(map[string]any{"t": "cfg", "q": c.Quality, "fps": c.FPS, "w": c.Width})
}

// RequestKeyframe asks for a fresh H.264 keyframe, as after a decoder error.
func (s *Session) RequestKeyframe() { s.sendJSON(map[string]any{"t": "kf"}) }

// MouseMove moves the remote pointer to (x, y), each 0–1 across the
// remote screen. Moves are coalesced to about 60 per second.
func (s *Session) MouseMove(x, y float64) {
	s.mu.Lock()
	s.pending = &msg{T: "mm", X: clamp(x), Y: clamp(y)}
	s.mu.Unlock()
}

// MouseButton presses or releases button b (0 left, 1 middle, 2 right).
func (s *Session) MouseButton(b int, down bool, x, y float64) {
	if down {
		s.cancelPaste()
	}
	s.flushMove()
	t := "mu"
	if down {
		t = "md"
	}
	s.send(msg{T: t, B: b, X: clamp(x), Y: clamp(y)})
}

// Wheel scrolls by pixels, browser-style: positive dy scrolls down.
func (s *Session) Wheel(dx, dy float64) {
	if dx == 0 && dy == 0 {
		return
	}
	s.flushMove()
	s.send(msg{T: "wh", DX: dx, DY: dy})
}

// Key presses or releases a key by its KeyboardEvent.code.
func (s *Session) Key(code string, down bool) {
	s.keyMu.Lock()
	defer s.keyMu.Unlock()
	s.key(code, down)
}

// key is called with keyMu held. Ignore a late key-up after focus loss.
func (s *Session) key(code string, down bool) {
	if code == "" {
		return
	}
	if down {
		s.pasteSeq++
		if s.requestPaste(code) {
			return
		}
		s.down[code] = true
		s.send(msg{T: "kd", C: code})
	} else if s.down[code] {
		delete(s.down, code)
		s.send(msg{T: "ku", C: code})
	}
}

// SendKeys presses a combination and releases it in reverse order.
// A modifier already held on the physical keyboard stays down.
func (s *Session) SendKeys(codes ...string) {
	s.keyMu.Lock()
	defer s.keyMu.Unlock()
	var pressed []string
	for _, c := range codes {
		if !s.down[c] {
			s.key(c, true)
			pressed = append(pressed, c)
		}
	}
	for i := len(pressed) - 1; i >= 0; i-- {
		s.key(pressed[i], false)
	}
}

// ReleaseKeys releases every key still held, as when the view loses focus.
func (s *Session) ReleaseKeys() {
	s.keyMu.Lock()
	defer s.keyMu.Unlock()
	s.pasteSeq++
	for c := range s.down {
		s.key(c, false)
	}
}

func (s *Session) SendClipboard(m clipshare.Message) { s.sendJSON(m.Packet()) }

// Text types s on the host as Unicode text, whatever its keyboard layout.
func (s *Session) Text(text string) {
	if text != "" {
		s.cancelPaste()
		s.sendJSON(map[string]any{"t": "tx", "s": text})
	}
}

// Close ends the session.
func (s *Session) Close() { s.finish(nil) }

// Done is closed once the session has ended.
func (s *Session) Done() <-chan struct{} { return s.ctx.Done() }

func (s *Session) finish(err error) {
	s.once.Do(func() {
		s.cancel()
		if err != nil {
			s.c.CloseNow() // the peer is gone: no close handshake to wait for
		} else {
			s.c.Close(websocket.StatusNormalClosure, "")
		}
		if s.h.OnClose != nil {
			s.h.OnClose(err)
		}
	})
}

func (s *Session) flushMove() {
	s.mu.Lock()
	m := s.pending
	s.pending = nil
	s.mu.Unlock()
	if m != nil {
		s.send(*m)
	}
}

func (s *Session) send(m msg) { s.sendJSON(m) }

func (s *Session) sendJSON(v any) {
	if s.ctx.Err() != nil {
		return
	}
	b, _ := json.Marshal(v)
	ctx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
	defer cancel()
	if err := s.c.Write(ctx, websocket.MessageText, b); err != nil && s.ctx.Err() == nil {
		go s.finish(err)
	}
}

func (s *Session) read() {
	for {
		typ, b, err := s.c.Read(s.ctx)
		s.seen.Store(time.Now().UnixNano())
		if err != nil {
			if s.ctx.Err() != nil || websocket.CloseStatus(err) == websocket.StatusNormalClosure {
				err = nil
			}
			s.finish(err)
			return
		}
		if typ == websocket.MessageBinary {
			if len(b) < 2 {
				continue
			}
			s.frames.Add(1)
			s.bytes.Add(int64(len(b)))
			if s.h.OnFrame != nil {
				s.h.OnFrame(b[0], b[1:])
			}
			continue
		}
		var m struct {
			clipshare.Message
			T     string  `json:"t"`
			Codec string  `json:"codec"`
			W     int     `json:"w"`
			H     int     `json:"h"`
			Msg   string  `json:"msg"`
			TS    float64 `json:"ts"`
			Info
		}
		if json.Unmarshal(b, &m) != nil {
			continue
		}
		switch m.T {
		case "info":
			s.mu.Lock()
			s.control = m.Control
			s.mu.Unlock()
			if s.h.OnInfo != nil {
				s.h.OnInfo(m.Info)
			}
		case "clip":
			if s.h.OnClipboard != nil && (len(m.Data) == 0 || clipshare.Valid(m.Data)) {
				s.h.OnClipboard(m.Message)
			}
		case "video":
			codec := "JPEG"
			if m.Codec == "h264" {
				codec = "H.264"
			}
			s.mu.Lock()
			s.codec = codec
			s.mu.Unlock()
			if s.h.OnVideo != nil {
				s.h.OnVideo(codec, m.W, m.H)
			}
		case "pong":
			rtt := time.Since(time.Unix(0, int64(m.TS)))
			s.mu.Lock()
			s.rtt = rtt
			s.mu.Unlock()
		case "error":
			s.mu.Lock()
			s.lastErr = m.Msg
			s.mu.Unlock()
		}
	}
}

func (s *Session) tick() {
	move := time.NewTicker(16 * time.Millisecond)
	sec := time.NewTicker(time.Second)
	defer move.Stop()
	defer sec.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-move.C:
			s.flushMove()
		case <-sec.C:
			if time.Since(time.Unix(0, s.seen.Load())) > Timeout {
				s.finish(ErrTimeout)
				return
			}
			s.sendJSON(map[string]any{"t": "ping", "ts": float64(time.Now().UnixNano())})
			if s.h.OnStats != nil {
				s.mu.Lock()
				st := Stats{Codec: s.codec, RTT: s.rtt, Control: s.control, Error: s.lastErr}
				s.mu.Unlock()
				st.FPS = int(s.frames.Swap(0))
				st.Mbps = float64(s.bytes.Swap(0)) * 8 / 1e6
				s.h.OnStats(st)
			}
		}
	}
}

func clamp(v float64) float64 { return min(max(v, 0), 1) }
