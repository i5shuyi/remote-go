package liveupdate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/i5shuyi/remote-go/internal/backend"
	"github.com/nsgo-dev/nsgo/core"
)

// Protocol 1 is shared by old and new frontends during replacement. Add
// fields compatibly. Credentials travel only on the user-private channel.
type Request struct {
	Op          string
	Text        string
	Number, FPS int
	On          bool
	Window      WindowRequest
}
type WindowRequest struct {
	Helper, Code, Password, Relay, Local string
	Quality, FPS                         int
}
type WindowResult struct {
	Target backend.Target
	Error  string
}
type Reply struct {
	State    backend.StateDTO
	Version  string
	Windows  int
	Retiring bool
	Error    string
}
type service struct {
	be          *backend.App
	mu          sync.Mutex
	windows     int
	stopping    bool
	lastContact time.Time
	stop        context.CancelFunc
	ctx         context.Context
}

func Serve(parent context.Context) error {
	var previous string
	if err := json.NewDecoder(os.Stdin).Decode(&previous); err != nil {
		return err
	}
	ln, release, err := listenLocal()
	if err != nil {
		return err
	}
	defer release()
	ctx, stop := context.WithCancel(parent)
	defer stop()
	s := &service{be: backend.New(), ctx: ctx, stop: stop, lastContact: time.Now()}
	s.be.Verbose = os.Getenv("RG_VERBOSE") != ""
	s.be.StartWithPassword(previous)
	defer s.be.Stop()
	go func() { <-ctx.Done(); ln.Close() }()
	go s.reapWhenAbandoned()
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go s.serveConn(c)
	}
}

// If an installer or the main window crashes, retain active sessions. A
// worker without a frontend or sessions can exit when the grace period ends.
func (s *service) reapWhenAbandoned() {
	tick := time.NewTicker(10 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case now := <-tick.C:
			s.mu.Lock()
			if now.Sub(s.lastContact) > 2*time.Minute && s.retire() {
				s.stop()
			}
			s.mu.Unlock()
		}
	}
}
func (s *service) retire() bool {
	if s.windows != 0 || !s.be.DrainIfIdle() {
		return false
	}
	s.stopping = true
	return true
}
func (s *service) serveConn(c net.Conn) {
	defer c.Close()
	c.SetDeadline(time.Now().Add(65 * time.Second))
	var q Request
	if err := json.NewDecoder(io.LimitReader(c, 64<<10)).Decode(&q); err != nil {
		return
	}
	r := s.dispatch(q)
	json.NewEncoder(c).Encode(r)
	if r.Retiring {
		s.stop()
	}
}
func (s *service) dispatch(q Request) (r Reply) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopping {
		r.Error = "会话服务正在切换版本"
		return
	}
	s.lastContact = time.Now()
	r.Version = core.App().Version
	var err error
	switch q.Op {
	case "state":
	case "name":
		err = s.be.SetName(q.Text)
	case "password":
		err = s.be.SetFixedPassword(q.Text)
	case "refresh":
		s.be.RefreshPassword()
	case "control":
		err = s.be.SetAllowControl(q.On)
	case "relay":
		err = s.be.SetRelay(q.Number, q.Text)
	case "defaults":
		s.be.SetDefaults(q.Number, q.FPS)
	case "forget":
		s.be.ForgetRecent(q.Text)
	case "kick":
		s.be.KickViewers()
	case "open":
		state := s.be.State()
		w := q.Window
		if w.Local == "" && w.Code == state.DeviceCode {
			err = errors.New("不能连接本机")
			break
		}
		w.Relay, w.Quality, w.FPS = state.RelayAddress, state.Quality, state.FPS
		s.windows++
		s.mu.Unlock()
		err = s.openWindow(w)
		s.mu.Lock()
	case "retire":
		r.Retiring = s.retire()
	case "quit":
		s.stopping = true
		r.Retiring = true
	default:
		err = fmt.Errorf("unknown session operation %q", q.Op)
	}
	if err != nil {
		r.Error = err.Error()
	}
	r.State = s.be.State()
	r.Windows = s.windows
	return
}

// The service keeps stdin open for the lifetime of each window. It closes
// only on a real quit, so an installer can replace the main program.
func (s *service) openWindow(w WindowRequest) error {
	finished := func() { s.mu.Lock(); s.windows--; s.mu.Unlock() }
	helper, err := windowHelper(w.Helper)
	if err != nil {
		finished()
		return err
	}
	cmd := exec.CommandContext(s.ctx, helper, "session-window")
	cmd.Stderr = os.Stderr
	in, err := cmd.StdinPipe()
	if err != nil {
		finished()
		return err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		in.Close()
		finished()
		return err
	}
	if err = cmd.Start(); err != nil {
		in.Close()
		out.Close()
		finished()
		return err
	}
	ready := make(chan WindowResult, 1)
	go func() {
		defer finished()
		defer in.Close()
		var result WindowResult
		err := json.NewEncoder(in).Encode(w)
		if err == nil {
			err = json.NewDecoder(out).Decode(&result)
		}
		if err != nil {
			result.Error = err.Error()
			cmd.Process.Kill()
		}
		if result.Error == "" && w.Local == "" {
			s.be.Remember(result.Target)
		}
		ready <- result
		cmd.Wait()
	}()
	select {
	case r := <-ready:
		if r.Error != "" {
			return errors.New(r.Error)
		}
		return nil
	case <-time.After(50 * time.Second):
		cmd.Process.Kill()
		return errors.New("会话窗口启动超时")
	case <-s.ctx.Done():
		return context.Canceled
	}
}

func exchange(q Request) (r Reply, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	c, err := dialLocal(ctx)
	if err != nil {
		return r, err
	}
	defer c.Close()
	timeout := 60 * time.Second
	if q.Op == "state" {
		timeout = 3 * time.Second
	}
	c.SetDeadline(time.Now().Add(timeout))
	if err = json.NewEncoder(c).Encode(q); err != nil {
		return r, err
	}
	err = json.NewDecoder(c).Decode(&r)
	if err == nil && r.Error != "" {
		err = errors.New(r.Error)
	}
	return
}

func start(helper, password string) error {
	cmd := exec.Command(helper, "session-host")
	cmd.Stderr = os.Stderr
	in, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	if err = cmd.Start(); err != nil {
		in.Close()
		return err
	}
	err = json.NewEncoder(in).Encode(password)
	in.Close()
	go func() {
		if err := cmd.Wait(); err != nil {
			log.Printf("session host: %v", err)
		}
	}()
	return err
}

// Missing endpoints may be leftovers of an unclean shutdown. Permission or
// protocol errors must be shown to the user instead of spawning more hosts.
func missingEndpoint(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED)
}
