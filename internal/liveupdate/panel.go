package liveupdate

import (
	"errors"
	"path/filepath"
	"sync"
	"time"

	"github.com/nsgo-dev/nsgo/core"

	"github.com/i5shuyi/remote-go/internal/members"
)

// Panel talks only to the local session service. Serialization of a version
// change prevents settings operations from racing the old worker's exit.
type Panel struct {
	helper   string
	mu       sync.RWMutex
	stopped  bool
	password string // in memory only, until the replacement worker answers
	Report   func(error)
}

func NewPanel() (*Panel, error) {
	helper, err := Stage()
	if err != nil {
		return nil, err
	}
	return &Panel{helper: helper}, nil
}

// An update must not open a new window over an active remote desktop.
func (p *Panel) ResumingSessions() bool {
	r, err := exchange(Request{Op: "state"})
	return err == nil && r.Version != core.App().Version && (r.Windows > 0 || len(r.State.Viewers) > 0)
}

func (p *Panel) Sync() (Reply, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopped {
		return Reply{}, errors.New("会话服务已退出")
	}
	r, err := exchange(Request{Op: "state"})
	if err != nil && !missingEndpoint(err) {
		return r, err
	}
	if err == nil {
		if r.Version == core.App().Version || r.Windows > 0 || len(r.State.Viewers) > 0 {
			return r, nil
		}
		r, err = exchange(Request{Op: "retire"})
		if err != nil || !r.Retiring {
			return r, err
		}
		p.password = r.State.Password
		// The old worker acknowledges before it closes the listener and unlocks it.
	}
	deadline := time.Now().Add(5 * time.Second)
	nextStart := time.Now()
	for time.Now().Before(deadline) {
		if time.Now().After(nextStart) {
			if err = start(p.helper, p.password); err != nil {
				return Reply{}, err
			}
			nextStart = time.Now().Add(time.Second)
		}
		time.Sleep(80 * time.Millisecond)
		r, err = exchange(Request{Op: "state"})
		if err == nil {
			p.password = ""
			cleanOldHelpers(p.helper)
			return r, nil
		}
	}
	return Reply{}, err
}
func (p *Panel) call(q Request) (Reply, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.stopped {
		return Reply{}, errors.New("会话服务已退出")
	}
	return exchange(q)
}
func (p *Panel) command(q Request) error { _, err := p.call(q); return err }
func (p *Panel) report(q Request) {
	if err := p.command(q); err != nil && p.Report != nil {
		p.Report(err)
	}
}
func (p *Panel) Quit() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stopped = true
	_, err := exchange(Request{Op: "quit"})
	return err
}
func (p *Panel) SetName(name string) error { return p.command(Request{Op: "name", Text: name}) }
func (p *Panel) SetFixedPassword(password string) error {
	return p.command(Request{Op: "password", Text: password})
}
func (p *Panel) RefreshPassword() string {
	r, err := p.call(Request{Op: "refresh"})
	if err != nil && p.Report != nil {
		p.Report(err)
	}
	return r.State.Password
}
func (p *Panel) SetAllowControl(on bool) error { return p.command(Request{Op: "control", On: on}) }
func (p *Panel) SetRelay(region int, address string) error {
	return p.command(Request{Op: "relay", Number: region, Text: address})
}
func (p *Panel) SetDefaults(quality, fps int) {
	p.report(Request{Op: "defaults", Number: quality, FPS: fps})
}
func (p *Panel) ForgetRecent(code string) { p.report(Request{Op: "forget", Text: code}) }
func (p *Panel) KickViewers()             { p.report(Request{Op: "kick"}) }
func (p *Panel) PairWith(code, password string) error {
	return p.command(Request{Op: "pair", Text: code, Secret: password})
}
func (p *Panel) AnswerPrompt(id int, ok, inbound bool) error {
	return p.command(Request{Op: "answer", Answer: &Answer{ID: id, OK: ok, Inbound: inbound}})
}
func (p *Panel) SetMemberPolicy(pol members.Policy) error {
	return p.command(Request{Op: "member-policy", Policy: &pol})
}
func (p *Panel) RemoveMember(id string, rotate bool) error {
	return p.command(Request{Op: "member-remove", Text: id, On: rotate})
}
func (p *Panel) RotateMemberKey() error { return p.command(Request{Op: "member-rotate"}) }
func (p *Panel) SyncMembers()           { p.report(Request{Op: "member-sync"}) }
func (p *Panel) LeaveMembers() error    { return p.command(Request{Op: "member-leave"}) }
func (p *Panel) RebuildMembers() error  { return p.command(Request{Op: "member-rebuild"}) }
func (p *Panel) OpenMember(id string) error {
	return p.command(Request{Op: "open", Window: WindowRequest{Helper: filepath.Base(p.helper), Member: id}})
}
func (p *Panel) Open(code, password, local string) error {
	return p.command(Request{Op: "open", Window: WindowRequest{Helper: filepath.Base(p.helper), Code: code, Password: password, Local: local}})
}
