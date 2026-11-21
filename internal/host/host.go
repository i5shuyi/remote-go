// Package host is the controlled side: it captures the screen, streams JPEG
// frames over a WebSocket and injects the viewer's mouse/keyboard events.
// It listens only inside a tailcat tunnel (WireGuard), never on a public
// interface.
package host

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"hash/maphash"
	"image"
	"image/jpeg"
	"log"
	"math/big"
	"net"
	"net/http"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/kbinani/screenshot"
	"golang.org/x/image/draw"

	"github.com/i5shuyi/remote-go/internal/clipshare"
	"github.com/i5shuyi/remote-go/internal/display"
	"github.com/i5shuyi/remote-go/internal/encoder"
	"github.com/i5shuyi/remote-go/internal/input"
	"github.com/i5shuyi/remote-go/internal/webui"
)

// Binary frame types (first byte of every binary WebSocket message).
const (
	frameJPEG     = 1
	frameH264Key  = 2
	frameH264Diff = 3
)

// startVideo is swappable for tests.
var startVideo = encoder.StartH264

const Port = 7900

// captureRect is swappable for tests.
var captureRect = screenshot.CaptureRect

type Config struct {
	Name     string // friendly device name shown to viewers
	Code     string // access code; generated if empty
	Display  int    // display index
	ViewOnly bool   // ignore input events
	// OnChange, if set, is called whenever a viewer connects or disconnects.
	OnChange func()
	// PairToken, if set, issues a single-use pairing token to a controller
	// that proves the verification code ("my devices"). The /api/info
	// greeting then advertises "members": 1.
	PairToken func() []byte
}

// SessionInfo describes a connected viewer.
type SessionInfo struct {
	ID       int       `json:"id"`
	Node     string    `json:"node"`
	Login    string    `json:"login"`
	Since    time.Time `json:"since"`
	Member   string    `json:"member,omitempty"` // member ID for "my devices" sessions
	ViewOnly bool      `json:"viewOnly,omitempty"`
}

type liveSession struct {
	info   SessionInfo
	cancel context.CancelFunc
}

type Server struct {
	cfg       Config
	inj       input.Injector
	clipboard clipshare.Board
	capture   func(image.Rectangle) (*image.RGBA, error)

	mu          sync.Mutex
	fails       int
	lockedUntil time.Time
	active      atomic.Int32
	draining    bool
	nextID      int
	sessions    map[int]*liveSession
	// inputPaused is the local input barrier raised while a security
	// prompt is on screen: remote input is dropped, not queued.
	inputMu     sync.Mutex
	inputPaused atomic.Bool
}

func New(cfg Config) (*Server, error) {
	if cfg.Code == "" {
		cfg.Code = RandomCode()
	}
	s := &Server{cfg: cfg, capture: captureRect, sessions: map[int]*liveSession{}}
	display.Prepare(!cfg.ViewOnly)
	if !cfg.ViewOnly {
		inj, err := input.New()
		if err != nil {
			log.Printf("输入注入不可用（%v），以仅观看模式运行", err)
		}
		s.inj = inj
		if inj != nil {
			s.clipboard = clipshare.System()
		}
	}
	if n := display.Count(); n >= 0 && cfg.Display >= n {
		return nil, fmt.Errorf("显示器编号 %d 不存在（共 %d 个）", cfg.Display, n)
	}
	return s, nil
}

func (s *Server) Code() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg.Code
}

// SetCode changes the access code; existing sessions stay connected.
func (s *Server) SetCode(code string) {
	s.mu.Lock()
	s.cfg.Code = code
	s.mu.Unlock()
}

// CanControl reports whether input injection is available.
func (s *Server) CanControl() bool { return s.inj != nil }

// Sessions lists connected viewers.
func (s *Server) Sessions() []SessionInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]SessionInfo, 0, len(s.sessions))
	for _, ls := range s.sessions {
		out = append(out, ls.info)
	}
	return out
}

// Kick disconnects all viewers.
func (s *Server) Kick() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ls := range s.sessions {
		ls.cancel()
	}
}

// KickCodeSessions disconnects viewers that came in with the device code,
// leaving "my devices" sessions alone (the codes changed).
func (s *Server) KickCodeSessions() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ls := range s.sessions {
		if ls.info.Member == "" {
			ls.cancel()
		}
	}
}

// SetName changes the name new viewers see.
func (s *Server) SetName(name string) {
	s.mu.Lock()
	s.cfg.Name = name
	s.mu.Unlock()
}

func (s *Server) name() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg.Name
}

// KickMember disconnects the sessions of one member.
func (s *Server) KickMember(member string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ls := range s.sessions {
		if ls.info.Member == member {
			ls.cancel()
		}
	}
}

// InputPaused reports whether the input barrier is up.
func (s *Server) InputPaused() bool { return s.inputPaused.Load() }

// PauseInput raises or lowers the input barrier. Raising it releases every
// key and button remote sessions hold; events arriving meanwhile are
// discarded so none can answer a local prompt.
func (s *Server) PauseInput(on bool) {
	s.inputMu.Lock()
	defer s.inputMu.Unlock()
	s.inputPaused.Store(on)
	if on && s.inj != nil {
		s.inj.ReleaseAll()
	}
}

func (s *Server) changed() {
	if s.cfg.OnChange != nil {
		go s.cfg.OnChange()
	}
}

// RandomCode returns a 6-digit access code.
func RandomCode() string {
	n, _ := rand.Int(rand.Reader, big.NewInt(1_000_000))
	return fmt.Sprintf("%06d", n.Int64())
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /", http.FileServerFS(webui.HostFS()))
	mux.HandleFunc("GET /ws", s.handleWS)
	mux.HandleFunc("GET /api/info", s.handleInfo)
	if s.cfg.PairToken != nil {
		mux.HandleFunc("POST /api/pair-token", func(w http.ResponseWriter, r *http.Request) {
			if err := s.checkCode(r.URL.Query().Get("code")); err != nil {
				time.Sleep(500 * time.Millisecond)
				http.Error(w, err.Error(), http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"token": s.cfg.PairToken()})
		})
	}
	return mux
}

func (s *Server) handleInfo(w http.ResponseWriter, r *http.Request) {
	info := map[string]any{"name": s.name(), "os": runtime.GOOS, "control": s.inj != nil}
	if s.cfg.PairToken != nil {
		info["members"] = 1
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(info)
}

// MemberPeer is an authenticated, approved "my devices" controller.
type MemberPeer struct {
	ID       string // full member ID
	Name     string
	ViewOnly bool // input and clipboard are refused for the whole session
}

// ServeMember runs the desktop protocol on a connection the member layer
// has already authenticated and approved. It returns when the session
// ends or ctx is cancelled (revocation).
func (s *Server) ServeMember(ctx context.Context, conn net.Conn, peer MemberPeer) {
	ln := &oneConn{c: conn, done: make(chan struct{})}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/info", s.handleInfo)
	mux.HandleFunc("GET /ws", func(w http.ResponseWriter, r *http.Request) {
		defer ln.Close() // a hijacked WebSocket never transitions to http.StateClosed
		r = r.WithContext(ctx)
		s.acceptWS(w, r, SessionInfo{Node: peer.Name, Login: "我的设备", Member: peer.ID, ViewOnly: peer.ViewOnly})
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second,
		ConnState: func(_ net.Conn, st http.ConnState) {
			if st == http.StateClosed {
				ln.Close()
			}
		}}
	stop := context.AfterFunc(ctx, func() { srv.Close() })
	defer stop()
	srv.Serve(ln)
	<-ln.done
	srv.Close()
}

// oneConn is a listener that yields a single connection.
type oneConn struct {
	c    net.Conn
	once sync.Once
	used bool
	done chan struct{}
}

func (l *oneConn) Accept() (net.Conn, error) {
	if !l.used {
		l.used = true
		return l.c, nil
	}
	<-l.done
	return nil, net.ErrClosed
}
func (l *oneConn) Close() error   { l.once.Do(func() { close(l.done) }); return nil }
func (l *oneConn) Addr() net.Addr { return l.c.LocalAddr() }

func (s *Server) Serve(ln net.Listener) error {
	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	return srv.Serve(ln)
}

// who names the peer behind a connection: its address inside the tunnel.
// (tailcat peers have no account identity; the device + verification code
// that derived the tunnel key is the credential.)
func (s *Server) who(ctx context.Context, remote string) (login, node string) {
	return "", remote
}

func (s *Server) checkCode(code string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if time.Now().Before(s.lockedUntil) {
		return errors.New("尝试次数过多，请稍后再试")
	}
	if code != "" && subtle.ConstantTimeCompare([]byte(code), []byte(s.cfg.Code)) == 1 {
		s.fails = 0
		return nil
	}
	s.fails++
	if s.fails >= 5 {
		s.fails = 0
		s.lockedUntil = time.Now().Add(time.Minute)
	}
	return errors.New("访问码错误")
}

// DrainIfIdle prevents an update from closing a socket that has just
// authenticated but has not yet entered the session list.
func (s *Server) DrainIfIdle() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active.Load() != 0 {
		return false
	}
	s.draining = true
	return true
}

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	login, node := s.who(r.Context(), r.RemoteAddr)
	if err := s.checkCode(r.URL.Query().Get("code")); err != nil {
		log.Printf("拒绝 %s (%s)：%v", node, login, err)
		time.Sleep(500 * time.Millisecond)
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	s.acceptWS(w, r, SessionInfo{Node: node, Login: login})
}

// acceptWS runs an authenticated viewer's session.
func (s *Server) acceptWS(w http.ResponseWriter, r *http.Request, info SessionInfo) {
	login, node := info.Login, info.Node
	s.mu.Lock()
	if s.draining {
		s.mu.Unlock()
		http.Error(w, "host updating", http.StatusServiceUnavailable)
		return
	}
	n := s.active.Add(1)
	s.mu.Unlock()
	defer s.active.Add(-1)
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return
	}
	defer c.CloseNow()
	c.SetReadLimit(clipshare.ChunkSize * 2)
	log.Printf("▶ %s (%s) 已连接，当前 %d 个控制端", node, login, n)
	defer log.Printf("■ %s (%s) 已断开", node, login)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	s.mu.Lock()
	s.nextID++
	id := s.nextID
	info.ID, info.Since = id, time.Now()
	s.sessions[id] = &liveSession{info: info, cancel: cancel}
	s.mu.Unlock()
	s.changed()
	defer func() {
		s.mu.Lock()
		delete(s.sessions, id)
		s.mu.Unlock()
		s.changed()
	}()

	bounds, err := display.Bounds(ctx, s.cfg.Display)
	if err != nil {
		msg, _ := json.Marshal(map[string]string{"t": "error", "msg": "无法共享屏幕：" + err.Error()})
		c.Write(ctx, websocket.MessageText, msg)
		c.Close(websocket.StatusInternalError, "screen unavailable")
		return
	}
	sess := &session{s: s, c: c, bounds: bounds, viewOnly: info.ViewOnly,
		wantH264: r.URL.Query().Get("codec") == "h264",
		reconf:   make(chan struct{}, 1), kf: make(chan struct{}, 1)}
	sess.quality.Store(70)
	sess.fps.Store(15)
	sess.maxW.Store(1920)
	sess.run(ctx)
}

type session struct {
	clipboard clipshare.Host
	s         *Server
	c         *websocket.Conn
	bounds    image.Rectangle
	quality   atomic.Int32
	fps       atomic.Int32
	maxW      atomic.Int32
	kick      chan struct{}
	viewOnly  bool // no input or clipboard for this session

	wantH264 bool          // viewer can decode H.264 (WebCodecs)
	reconf   chan struct{} // quality/fps/size changed
	kf       chan struct{} // viewer asks for a keyframe
}

type inMsg struct {
	clipshare.Message
	T   string  `json:"t"`
	X   float64 `json:"x"`
	Y   float64 `json:"y"`
	B   int     `json:"b"`
	DX  float64 `json:"dx"`
	DY  float64 `json:"dy"`
	C   string  `json:"c"`
	S   string  `json:"s"`
	Q   int32   `json:"q"`
	FPS int32   `json:"fps"`
	W   int32   `json:"w"`
	TS  float64 `json:"ts"`
}

func (ss *session) run(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	ss.kick = make(chan struct{}, 1)

	_, rich := ss.s.clipboard.(clipshare.RichBoard)
	info, _ := json.Marshal(map[string]any{
		"t": "info", "name": ss.s.name(), "w": ss.bounds.Dx(), "h": ss.bounds.Dy(),
		"clipboardRich": rich && ss.canInput(), "control": ss.canInput(), "clipboard": ss.s.clipboard != nil && ss.canInput(),
	})
	if err := ss.c.Write(ctx, websocket.MessageText, info); err != nil {
		return
	}
	if ss.s.inj != nil {
		defer ss.s.inj.ReleaseAll()
	}
	go func() {
		defer cancel()
		defer ss.clipboard.Close()
		for {
			_, data, err := ss.c.Read(ctx)
			if err != nil {
				return
			}
			var m inMsg
			if json.Unmarshal(data, &m) == nil {
				ss.handle(ctx, m)
			}
		}
	}()
	ss.stream(ctx)
}

func (ss *session) canInput() bool { return ss.s.inj != nil && !ss.viewOnly }

func (ss *session) abs(x, y float64) (float64, float64) {
	x, y = clamp01(x), clamp01(y)
	return float64(ss.bounds.Min.X) + x*float64(ss.bounds.Dx()),
		float64(ss.bounds.Min.Y) + y*float64(ss.bounds.Dy())
}

func (ss *session) handle(ctx context.Context, m inMsg) {
	switch m.T {
	case "clip":
		if ctx.Err() == nil && ss.canInput() && !ss.s.cfg.ViewOnly && !ss.s.inputPaused.Load() {
			if reply := ss.clipboard.HandleContext(ctx, ss.s.clipboard, m.Message); reply != nil {
				ss.sendJSON(ctx, reply.Packet())
			}
		}
		return
	case "cfg":
		changed := false
		if m.Q >= 10 && m.Q <= 95 && m.Q != ss.quality.Load() {
			ss.quality.Store(m.Q)
			changed = true
		}
		if m.FPS >= 1 && m.FPS <= 60 && m.FPS != ss.fps.Load() {
			ss.fps.Store(m.FPS)
			changed = true
		}
		if m.W >= 320 && m.W <= 7680 && m.W != ss.maxW.Load() {
			ss.maxW.Store(m.W)
			changed = true
		}
		if changed {
			signal(ss.reconf)
		}
		return
	case "kf":
		signal(ss.kf)
		return
	case "ping":
		ss.sendJSON(context.Background(), map[string]any{"t": "pong", "ts": m.TS})
		return
	}
	ss.s.inputMu.Lock()
	defer ss.s.inputMu.Unlock()
	inj := ss.s.inj
	if !ss.canInput() || ss.s.inputPaused.Load() {
		return
	}
	switch m.T {
	case "mm":
		inj.MouseMove(ss.abs(m.X, m.Y))
	case "md", "mu":
		x, y := ss.abs(m.X, m.Y)
		inj.MouseButton(m.B, m.T == "md", x, y)
	case "wh":
		inj.Wheel(m.DX, m.DY)
	case "kd", "ku":
		inj.Key(m.C, m.T == "kd")
	case "tx":
		if len(m.S) <= 4096 {
			inj.Text(m.S)
		}
	default:
		return
	}
	// Input usually changes the screen; capture sooner.
	signal(ss.kick)
}

func signal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

func (ss *session) sendJSON(ctx context.Context, v any) error {
	b, _ := json.Marshal(v)
	return ss.c.Write(ctx, websocket.MessageText, b)
}

func (ss *session) sendFrame(ctx context.Context, typ byte, data []byte) error {
	buf := make([]byte, 1+len(data))
	buf[0] = typ
	copy(buf[1:], data)
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return ss.c.Write(wctx, websocket.MessageBinary, buf)
}

func (ss *session) stream(ctx context.Context) {
	var h264Err error
	if ss.wantH264 {
		err := ss.streamH264(ctx)
		if err == nil || ctx.Err() != nil {
			return
		}
		h264Err = err
		log.Printf("硬件编码不可用，回退到 JPEG：%v", err)
	}
	if ss.sendJSON(ctx, map[string]any{"t": "video", "codec": "jpeg"}) != nil {
		return
	}
	ss.streamJPEG(ctx, h264Err)
}

// streamH264 runs the hardware pipeline. It returns an error (so the caller
// falls back to JPEG) only if the encoder cannot start or stops by itself.
func (ss *session) streamH264(ctx context.Context) error {
	for {
		v, err := startVideo(encoder.Options{Display: ss.s.cfg.Display, MaxWidth: int(ss.maxW.Load()),
			FPS: int(ss.fps.Load()), Quality: int(ss.quality.Load())})
		if err != nil {
			return err
		}
		w, h := v.Size()
		if err := ss.sendJSON(ctx, map[string]any{"t": "video", "codec": "h264", "w": w, "h": h}); err != nil {
			v.Close()
			return nil
		}
		restart, err := ss.pumpH264(ctx, v)
		v.Close()
		if !restart {
			return err
		}
	}
}

func (ss *session) pumpH264(ctx context.Context, v encoder.Video) (restart bool, err error) {
	for {
		select {
		case <-ctx.Done():
			return false, nil
		case <-v.Done():
			return false, v.Err()
		case <-ss.reconf:
			return true, nil
		case <-ss.kf:
			v.RequestKeyframe()
		case f := <-v.Frames():
			typ := byte(frameH264Diff)
			if f.Key {
				typ = frameH264Key
			}
			if ss.sendFrame(ctx, typ, f.Data) != nil {
				return false, nil
			}
		}
	}
}

func (ss *session) streamJPEG(ctx context.Context, h264Err error) {
	var (
		seed     = maphash.MakeSeed()
		lastHash uint64
		lastSent time.Time
		buf      bytes.Buffer
		scaled   *image.RGBA
		errShown bool
	)
	for ctx.Err() == nil {
		start := time.Now()
		img, err := ss.s.capture(ss.bounds)
		if err != nil {
			if !errShown {
				errShown = true
				log.Printf("截屏失败: %v", err)
				detail := "截屏失败: " + err.Error()
				if h264Err != nil {
					detail = fmt.Sprintf("H.264：%v；JPEG：%v", h264Err, err)
				}
				msg, _ := json.Marshal(map[string]string{"t": "error", "msg": detail})
				ss.c.Write(ctx, websocket.MessageText, msg)
			}
		} else {
			h := maphash.Bytes(seed, img.Pix)
			// Skip unchanged frames, but resend periodically as a keepalive.
			if h != lastHash || time.Since(lastSent) > 3*time.Second {
				lastHash = h
				var src image.Image = img
				if mw := int(ss.maxW.Load()); img.Bounds().Dx() > mw {
					nh := img.Bounds().Dy() * mw / img.Bounds().Dx()
					if scaled == nil || scaled.Bounds().Dx() != mw || scaled.Bounds().Dy() != nh {
						scaled = image.NewRGBA(image.Rect(0, 0, mw, nh))
					}
					draw.ApproxBiLinear.Scale(scaled, scaled.Bounds(), img, img.Bounds(), draw.Src, nil)
					src = scaled
				}
				buf.Reset()
				if err := jpeg.Encode(&buf, src, &jpeg.Options{Quality: int(ss.quality.Load())}); err == nil {
					if ss.sendFrame(ctx, frameJPEG, buf.Bytes()) != nil {
						return
					}
					lastSent = time.Now()
				}
			}
		}
		interval := time.Second / time.Duration(max(ss.fps.Load(), 1))
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval - time.Since(start)):
		case <-ss.kick:
			// small delay so the UI has a moment to react to the input
			time.Sleep(15 * time.Millisecond)
		}
	}
}

func clamp01(v float64) float64 { return min(max(v, 0), 1) }

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
