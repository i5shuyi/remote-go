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
	"sync/atomic"
	"syscall"
	"time"

	"github.com/i5shuyi/remote-go/internal/backend"
	"github.com/i5shuyi/remote-go/internal/members"
	"github.com/nsgo-dev/nsgo/core"
)

// Protocol 1 is shared by old and new frontends during replacement. Add
// fields compatibly. Credentials travel only on the user-private channel.
type Request struct {
	Op          string
	Text        string
	Secret      string // a verification code, for pairing
	Number, FPS int
	On          bool
	Window      WindowRequest
	Answer      *Answer         `json:",omitempty"`
	Policy      *members.Policy `json:",omitempty"`
}

// Proxy is a one-shot loopback connection to an approved member session.
type Proxy struct{ Addr, Token, Name string }

// Answer is the person's reply to a local prompt.
type Answer struct {
	ID      int
	OK      bool
	Inbound bool
}

type WindowRequest struct {
	Helper, Code, Password, Relay, Local string
	Quality, FPS                         int
	// Member asks the worker to dial a "my devices" member; the window
	// then runs the session through the worker's one-shot proxy.
	Member                              string
	MemberAddr, MemberToken, MemberName string
}
type WindowResult struct {
	Target backend.Target
	Error  string
}
type Reply struct {
	Proxy    *Proxy `json:",omitempty"`
	State    backend.StateDTO
	Version  string
	Windows  int
	Retiring bool
	Error    string
}
type service struct {
	pendingChanges int // protected by mu
	be             *backend.App
	mu             sync.Mutex
	windows        int
	stopping       bool
	lastContact    time.Time
	lastUI         atomic.Int64 // UnixNano of the last main-window poll
	stop           context.CancelFunc
	ctx            context.Context
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
	// Local prompts are drawn by the main window, which polls every second.
	s.be.UIAttached = func() bool { return time.Since(time.Unix(0, s.lastUI.Load())) < 5*time.Second }
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
	if s.windows != 0 || s.pendingChanges != 0 || !s.be.DrainIfIdle() {
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
		s.lastUI.Store(time.Now().UnixNano())
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
	case "pair":
		err = s.be.PairWith(q.Text, q.Secret)
	case "answer":
		if q.Answer != nil {
			err = s.be.AnswerPrompt(q.Answer.ID, q.Answer.OK, q.Answer.Inbound)
		}
	case "member-proxy": // a session window reconnecting
		s.mu.Unlock()
		var p Proxy
		p.Addr, p.Token, p.Name, err = s.be.MemberProxy(q.Text)
		s.mu.Lock()
		if err == nil {
			r.Proxy = &p
		}
	case "member-sync":
		s.be.SyncMembers()
	case "member-policy", "member-remove", "member-rotate", "member-leave", "member-rebuild":
		// The window must be able to poll and answer while the operation waits.
		s.pendingChanges++
		s.mu.Unlock()
		switch q.Op {
		case "member-policy":
			if q.Policy != nil {
				err = s.be.SetMemberPolicy(*q.Policy)
			}
		case "member-remove":
			err = s.be.RemoveMember(q.Text, q.On)
		case "member-rotate":
			err = s.be.RotateMemberKey()
		case "member-leave":
			err = s.be.LeaveMembers()
		case "member-rebuild":
			err = s.be.RebuildMembers()
		}
		s.mu.Lock()
		s.pendingChanges--
	case "open":
		state := s.be.State()
		w := q.Window
		if w.Local == "" && w.Member == "" && w.Code == state.DeviceCode {
			err = errors.New("不能连接本机")
			break
		}
		w.Relay, w.Quality, w.FPS = state.RelayAddress, state.Quality, state.FPS
		s.windows++
		s.mu.Unlock()
		if w.Member != "" {
			// Sync and the target's approval happen here, before the window.
			w.MemberAddr, w.MemberToken, w.MemberName, err = s.be.MemberProxy(w.Member)
		}
		if err == nil {
			err = s.openWindow(w)
		} else {
			s.mu.Lock()
			s.windows--
			s.mu.Unlock()
		}
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
	if q.Op == "open" && q.Window.Member != "" || q.Op == "member-proxy" {
		timeout = members.ApproveTimeout + 80*time.Second
	}
	if q.Op == "member-leave" {
		timeout = members.ApproveTimeout + 35*time.Second
	}
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

// RequestMemberProxy asks the worker for a new member session, from a
// session window.
func RequestMemberProxy(member string) (Proxy, error) {
	r, err := exchange(Request{Op: "member-proxy", Text: member})
	if err != nil {
		return Proxy{}, err
	}
	if r.Proxy == nil {
		return Proxy{}, errors.New("会话服务未返回连接")
	}
	return *r.Proxy, nil
}
