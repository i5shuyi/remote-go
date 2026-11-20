//go:build linux && !android

package portal

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
)

const (
	busName      = "org.freedesktop.portal.Desktop"
	objectPath   = "/org/freedesktop/portal/desktop"
	ifScreenCast = "org.freedesktop.portal.ScreenCast"
	ifRemote     = "org.freedesktop.portal.RemoteDesktop"
	ifRequest    = "org.freedesktop.portal.Request"
	ifSession    = "org.freedesktop.portal.Session"

	sourceMonitor       = 1
	devKeyboard         = 1
	devPointer          = 2
	cursorEmbedded      = 2
	cursorHidden        = 1
	persistUntilRevoked = 2

	// How long Get waits for the person to answer the portal's dialog.
	consentTimeout = 3 * time.Minute
	// A refused or failed start is not retried sooner, so reconnecting
	// viewers do not raise a dialog after every attempt.
	retryAfter = 30 * time.Second
)

// Session is a started portal session. Its methods may be called from
// any goroutine.
type Session struct {
	conn    *dbus.Conn
	handle  dbus.ObjectPath
	input   bool
	streams []Stream
	closed  chan struct{}
	once    sync.Once
}

var state struct {
	mu       sync.Mutex
	input    bool          // whether sessions take input
	current  *Session      // the running session
	starting chan struct{} // closed when a start finishes
	err      error         // the last start's error
	failedAt time.Time
}

// Prepare starts a session in the background, so the portal's dialog
// shows while the person who launched the host is still at the machine.
// input asks for keyboard and pointer as well as the screen.
func Prepare(input bool) {
	state.mu.Lock()
	state.input = state.input || input
	state.mu.Unlock()
	go Get(context.Background())
}

// Current returns the running session without starting one.
func Current() *Session {
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.current
}

// Get returns the running session, starting one (and waiting for the
// person to allow it) if there is none.
func Get(ctx context.Context) (*Session, error) {
	for {
		state.mu.Lock()
		if s := state.current; s != nil {
			state.mu.Unlock()
			return s, nil
		}
		if wait := state.starting; wait != nil {
			state.mu.Unlock()
			select {
			case <-wait:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		if state.err != nil && time.Since(state.failedAt) < retryAfter {
			err := state.err
			state.mu.Unlock()
			return nil, err
		}
		done := make(chan struct{})
		state.starting = done
		input := state.input
		state.mu.Unlock()

		s, err := start(input)
		state.mu.Lock()
		state.starting = nil
		state.current, state.err = s, err
		if err != nil {
			state.failedAt = time.Now()
			log.Printf("屏幕共享会话未启动：%v", err)
		}
		state.mu.Unlock()
		close(done)
		if s != nil {
			go func() {
				<-s.closed
				state.mu.Lock()
				if state.current == s {
					state.current = nil
				}
				state.mu.Unlock()
			}()
		}
		return s, err
	}
}

// Streams lists the shared monitors in the order the person chose them.
func (s *Session) Streams() []Stream { return s.streams }

// Input reports whether the session takes keyboard and pointer input.
func (s *Session) Input() bool { return s.input }

// Done is closed when the compositor ends the session.
func (s *Session) Done() <-chan struct{} { return s.closed }

// PipeWire returns a new file descriptor connected to the session's
// PipeWire remote; the caller owns it.
func (s *Session) PipeWire() (int, error) {
	var fd dbus.UnixFD
	err := s.conn.Object(busName, objectPath).Call(ifScreenCast+".OpenPipeWireRemote", 0,
		s.handle, map[string]dbus.Variant{}).Store(&fd)
	if err != nil {
		return -1, err
	}
	return int(fd), nil
}

func (s *Session) notify(method string, args ...any) {
	s.notifyWith(method, map[string]dbus.Variant{}, args...)
}

func (s *Session) notifyWith(method string, options map[string]dbus.Variant, args ...any) {
	if !s.input {
		return
	}
	select {
	case <-s.closed:
		return
	default:
	}
	all := append([]any{s.handle, options}, args...)
	// Input must not wait on the bus: send without a reply.
	s.conn.Object(busName, objectPath).Go(ifRemote+"."+method, dbus.FlagNoReplyExpected, nil, all...)
}

// Motion moves the pointer to (x, y) in stream's logical coordinates.
func (s *Session) Motion(stream uint32, x, y float64) {
	s.notify("NotifyPointerMotionAbsolute", stream, x, y)
}

// Button presses or releases an evdev button (BTN_LEFT is 0x110).
func (s *Session) Button(button int32, down bool) {
	s.notify("NotifyPointerButton", button, boolState(down))
}

// Axis scrolls by (dx, dy) logical pixels.
func (s *Session) Axis(dx, dy float64) {
	s.notifyWith("NotifyPointerAxis", map[string]dbus.Variant{"finish": dbus.MakeVariant(true)}, dx, dy)
}

// Key presses or releases an evdev keycode.
func (s *Session) Key(code int32, down bool) {
	s.notify("NotifyKeyboardKeycode", code, boolState(down))
}

// Keysym presses or releases an X keysym, which the compositor maps to a
// key of the current layout.
func (s *Session) Keysym(sym int32, down bool) {
	s.notify("NotifyKeyboardKeysym", sym, boolState(down))
}

func boolState(down bool) uint32 {
	if down {
		return 1
	}
	return 0
}

func (s *Session) close() { s.once.Do(func() { close(s.closed) }) }

// start runs the portal's handshake: CreateSession, SelectDevices (for
// input), SelectSources, Start.
func start(input bool) (*Session, error) {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return nil, fmt.Errorf("%w：%v", ErrUnavailable, err)
	}
	p := &porter{conn: conn, obj: conn.Object(busName, objectPath), waiters: map[dbus.ObjectPath]chan *dbus.Signal{}}
	sigs := make(chan *dbus.Signal, 16)
	conn.Signal(sigs)
	s := &Session{conn: conn, input: input, closed: make(chan struct{})}
	go p.dispatch(sigs, s)
	ok := false
	defer func() {
		if !ok {
			conn.Close()
		}
	}()

	iface, kind := ifScreenCast, "screencast"
	if input {
		iface, kind = ifRemote, "remote-desktop"
	}
	tokenFile := tokenPath(kind)
	restore, _ := os.ReadFile(tokenFile)

	res, err := p.request(iface+".CreateSession", map[string]dbus.Variant{
		"session_handle_token": dbus.MakeVariant(token()),
	})
	if err != nil {
		return nil, err
	}
	var handle string
	if v, found := res["session_handle"]; !found || v.Store(&handle) != nil {
		return nil, errors.New("门户没有返回会话")
	}
	s.handle = dbus.ObjectPath(handle)
	conn.AddMatchSignal(dbus.WithMatchObjectPath(s.handle), dbus.WithMatchInterface(ifSession), dbus.WithMatchMember("Closed"))

	sources := map[string]dbus.Variant{
		"types":       dbus.MakeVariant(uint32(sourceMonitor)),
		"multiple":    dbus.MakeVariant(true),
		"cursor_mode": dbus.MakeVariant(uint32(p.cursorMode())),
	}
	if input {
		devices := map[string]dbus.Variant{
			"types":        dbus.MakeVariant(uint32(devKeyboard | devPointer)),
			"persist_mode": dbus.MakeVariant(uint32(persistUntilRevoked)),
		}
		if len(restore) > 0 {
			devices["restore_token"] = dbus.MakeVariant(strings.TrimSpace(string(restore)))
		}
		if _, err := p.request(ifRemote+".SelectDevices", devices, s.handle); err != nil {
			return nil, err
		}
	} else {
		sources["persist_mode"] = dbus.MakeVariant(uint32(persistUntilRevoked))
		if len(restore) > 0 {
			sources["restore_token"] = dbus.MakeVariant(strings.TrimSpace(string(restore)))
		}
	}
	if _, err := p.request(ifScreenCast+".SelectSources", sources, s.handle); err != nil {
		return nil, err
	}
	res, err = p.requestTimeout(consentTimeout, iface+".Start", map[string]dbus.Variant{}, s.handle, "")
	if err != nil {
		return nil, err
	}
	if v, found := res["restore_token"]; found {
		var t string
		if v.Store(&t) == nil && t != "" {
			os.MkdirAll(filepath.Dir(tokenFile), 0o700)
			os.WriteFile(tokenFile, []byte(t), 0o600)
		}
	}
	if input {
		var devices uint32
		if v, found := res["devices"]; found {
			v.Store(&devices)
		}
		s.input = devices&(devKeyboard|devPointer) != 0
	}
	if s.streams, err = parseStreams(res["streams"]); err != nil {
		return nil, err
	}
	ok = true
	return s, nil
}

// porter sends portal requests and waits for their Response signals.
type porter struct {
	conn    *dbus.Conn
	obj     dbus.BusObject
	mu      sync.Mutex
	waiters map[dbus.ObjectPath]chan *dbus.Signal
}

func (p *porter) dispatch(sigs chan *dbus.Signal, s *Session) {
	for sig := range sigs {
		switch sig.Name {
		case ifRequest + ".Response":
			p.mu.Lock()
			ch := p.waiters[sig.Path]
			delete(p.waiters, sig.Path)
			p.mu.Unlock()
			if ch != nil {
				ch <- sig
			}
		case ifSession + ".Closed":
			if sig.Path == s.handle {
				s.close()
				p.conn.Close() // ends this loop
			}
		}
	}
	s.close() // the bus connection went away
}

func (p *porter) request(method string, options map[string]dbus.Variant, args ...any) (map[string]dbus.Variant, error) {
	return p.requestTimeout(30*time.Second, method, options, args...)
}

// requestTimeout calls method with args followed by options, then waits
// for the request's response. The request path is derived from our bus
// name and handle token, and subscribed to before the call, so a quick
// response is never missed.
func (p *porter) requestTimeout(timeout time.Duration, method string, options map[string]dbus.Variant, args ...any) (map[string]dbus.Variant, error) {
	tok := token()
	options["handle_token"] = dbus.MakeVariant(tok)
	sender := strings.ReplaceAll(strings.TrimPrefix(p.conn.Names()[0], ":"), ".", "_")
	path := dbus.ObjectPath(objectPath + "/request/" + sender + "/" + tok)
	ch := make(chan *dbus.Signal, 1)
	p.mu.Lock()
	p.waiters[path] = ch
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		delete(p.waiters, path)
		p.mu.Unlock()
	}()
	p.conn.AddMatchSignal(dbus.WithMatchObjectPath(path), dbus.WithMatchInterface(ifRequest), dbus.WithMatchMember("Response"))
	defer p.conn.RemoveMatchSignal(dbus.WithMatchObjectPath(path), dbus.WithMatchInterface(ifRequest), dbus.WithMatchMember("Response"))

	var handle dbus.ObjectPath
	// CreateSession takes only options; the others put the session first.
	call := p.obj.Call(method, 0, append(args, options)...)
	if call.Err != nil {
		return nil, fmt.Errorf("%w：%s：%v", ErrUnavailable, method[strings.LastIndex(method, ".")+1:], call.Err)
	}
	call.Store(&handle)
	if handle != "" && handle != path {
		// An old portal picked its own path; follow it.
		p.mu.Lock()
		delete(p.waiters, path)
		p.waiters[handle] = ch
		p.mu.Unlock()
		p.conn.AddMatchSignal(dbus.WithMatchObjectPath(handle), dbus.WithMatchInterface(ifRequest), dbus.WithMatchMember("Response"))
	}
	select {
	case sig := <-ch:
		var code uint32
		var results map[string]dbus.Variant
		if err := dbus.Store(sig.Body, &code, &results); err != nil {
			return nil, err
		}
		switch code {
		case 0:
			return results, nil
		case 1:
			return nil, errors.New("对方拒绝了屏幕共享")
		}
		return nil, errors.New("屏幕共享请求失败")
	case <-time.After(timeout):
		return nil, errors.New("等待屏幕共享授权超时")
	}
}

// cursorMode draws the pointer into the stream when the portal can.
func (p *porter) cursorMode() uint32 {
	v, err := p.obj.GetProperty(ifScreenCast + ".AvailableCursorModes")
	var modes uint32
	if err == nil && v.Store(&modes) == nil && modes&cursorEmbedded != 0 {
		return cursorEmbedded
	}
	return cursorHidden
}

func token() string {
	b := make([]byte, 8)
	rand.Read(b)
	return "remotego_" + hex.EncodeToString(b)
}

func tokenPath(kind string) string {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "remote-go", "portal-"+kind+".token")
}
