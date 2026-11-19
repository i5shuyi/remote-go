// Package sessionclip connects one native session's opt-in clipboard switch
// to its peer. All methods run on the UI thread, including posted replies.
package sessionclip

import (
	"context"
	"crypto/sha256"
	"errors"
	"time"

	"github.com/nsgo-dev/nsgo/core"

	"github.com/i5shuyi/remote-go/internal/client"
	"github.com/i5shuyi/remote-go/internal/clipshare"
)

type stamp struct {
	version uint64            // native clipboard change count
	digest  [sha256.Size]byte // Android exposes text through the UI bridge
}

type Controller struct {
	Status             *core.Signal[string]
	Rich               bool
	cancel             context.CancelFunc
	replies            chan clipshare.Message
	report             func(error)
	waiting            bool // one poll at a time; unsolicited replies never touch the clipboard
	cx                 *core.Scope
	session            func() *client.Session
	board              clipshare.Board
	Available, Enabled *core.Signal[bool]
	gen                uint64
	last, sent         stamp
	pending            []byte // a remote write to retry while the local clipboard is busy
}

func New(cx *core.Scope, session func() *client.Session, report func(error)) *Controller {
	return &Controller{Status: core.NewSignal(cx, ""), report: report, cx: cx, session: session, board: clipshare.System(),
		Available: core.NewSignal(cx, false), Enabled: core.NewSignal(cx, false)}
}

func (c *Controller) SetAvailable(yes bool) {
	c.Available.Set(yes)
	if !yes {
		c.Set(false)
	}
}

func (c *Controller) active(gen uint64) bool {
	if c.gen != gen || !c.Enabled.GetUntracked() {
		return false
	}
	s := c.session()
	if s == nil {
		return false
	}
	select {
	case <-s.Done():
		return false
	default:
		return true
	}
}

func (c *Controller) Set(on bool) {
	if on == c.Enabled.GetUntracked() || on && (!c.Available.GetUntracked() || c.session() == nil) {
		return
	}
	if c.cancel != nil {
		c.cancel()
		if s := c.session(); s != nil {
			s.StopPasteSync()
		}
		c.cancel = nil
		c.replies = nil
	}
	old := c.gen
	c.gen++
	c.Enabled.Set(on)
	c.Status.Set("")
	c.pending = nil
	c.waiting = false
	s := c.session()
	if !on {
		if s != nil {
			s.SendClipboard(clipshare.Message{Generation: old, Operation: "off"})
		}
		return
	}
	gen := c.gen
	if !c.active(gen) {
		c.Enabled.Set(false)
		return
	}
	if board, ok := c.board.(clipshare.RichBoard); ok && c.Rich {
		ctx, cancel := context.WithCancel(context.Background())
		replies := make(chan clipshare.Message, 1)
		c.cancel, c.replies = cancel, replies
		peer := clipshare.NewPeer(board)
		s.SendClipboard(clipshare.Message{Generation: gen, Operation: "on", Rich: true})
		go c.richLoop(ctx, s, gen, peer, replies, s.StartPasteSync())
		return
	}
	s.SendClipboard(clipshare.Message{Generation: gen, Operation: "on"})
	// Post to the session's owner: closing the menu must not dispose the read.
	c.cx.Post(func() {
		if !c.active(gen) {
			return
		}
		c.snapshot(gen, false, func(at stamp, _ []byte) {
			c.last = at
			c.schedule(gen)
		})
	})
}

// snapshot avoids fetching native pasteboard contents unless they changed.
// Android reads the current text asynchronously and compares fingerprints.
func (c *Controller) snapshot(gen uint64, readChange bool, done func(stamp, []byte)) {
	if c.board != nil {
		at := stamp{version: c.board.Version()}
		var data []byte
		if readChange && at != c.last {
			var ok bool
			data, ok = c.board.Read()
			if !ok {
				at = c.last
			} // retry on the next poll
		}
		done(at, data)
		return
	}
	c.cx.HasClipboardText(func(has bool) {
		if !c.active(gen) {
			return
		}
		if !has {
			done(stamp{}, nil)
			return
		}
		c.cx.ReadClipboard(func(text string) {
			if !c.active(gen) {
				return
			}
			if len(text) == 0 || len(text) > clipshare.MaxBytes {
				done(stamp{}, nil)
				return
			}
			b := []byte(text)
			done(stamp{digest: sha256.Sum256(b)}, b)
		})
	})
}

func (c *Controller) schedule(gen uint64) {
	c.cx.AfterFunc(400*time.Millisecond, func() {
		if !c.active(gen) {
			return
		}
		c.snapshot(gen, true, func(at stamp, b []byte) {
			m := clipshare.Message{Generation: gen, Operation: "poll"}
			if at != c.last {
				c.pending = nil // a newer local copy takes precedence
				if clipshare.Valid(b) {
					m.Data = b
				}
			}
			if len(c.pending) > 0 && !c.apply(c.pending) {
				c.schedule(gen)
				return
			}
			if c.pending != nil {
				at = c.last
				c.pending = nil
			}
			c.sent = at
			c.waiting = true
			c.session().SendClipboard(m)
		})
	})
}

func (c *Controller) apply(b []byte) bool {
	if c.board != nil {
		if !c.board.Write(b) {
			return false
		}
		c.last = stamp{version: c.board.Version()}
	} else {
		c.cx.SetClipboard(string(b))
		c.last = stamp{digest: sha256.Sum256(b)}
	}
	return true
}

func (c *Controller) Receive(m clipshare.Message) {
	if c.replies != nil && m.Operation == "result" && c.active(m.Generation) {
		select {
		case c.replies <- m:
		default:
		}
		return
	}
	if !c.waiting || !c.active(m.Generation) || (m.Operation != "result" && m.Operation != "retry") {
		return
	}
	c.waiting = false
	if m.Operation == "result" {
		c.last = c.sent
		if clipshare.Valid(m.Data) {
			c.snapshot(m.Generation, false, func(at stamp, _ []byte) {
				if at == c.last && !c.apply(m.Data) {
					c.pending = m.Data
				}
				c.schedule(m.Generation)
			})
			return
		}
	}
	c.schedule(m.Generation)
}

func (c *Controller) richLoop(ctx context.Context, s *client.Session, gen uint64, p *clipshare.Peer, replies <-chan clipshare.Message, pastes <-chan uint64) {
	defer p.Close()
	abort := false
	var paste uint64
	var status string
	failed := false
	for {
		select {
		case paste = <-pastes:
		default:
		}
		m := clipshare.Message{Generation: gen, Operation: "poll", Abort: abort}
		abort = false
		if !m.Abort {
			if err := p.Next(ctx, &m); err != nil {
				p.Close()
				m.Abort = true
				failed = true
				status = ""
				paste = 0
				c.showError(gen, err)
			}
		}
		if ctx.Err() != nil {
			return
		}
		if m.Part != nil && m.Part.Manifest != nil {
			status = "图片"
			if m.Part.Manifest.Kind == "files" {
				status = "文件"
			}
			c.setStatus(gen, "正在向远端传输"+status+"…")
		}
		s.SendClipboard(m)
		select {
		case <-ctx.Done():
			return
		case <-s.Done():
			return
		case reply := <-replies:
			if ctx.Err() != nil {
				return
			}
			if err := p.Apply(ctx, reply, true); err != nil {
				p.Close()
				abort = true
				failed = true
				status = ""
				paste = 0
				c.showError(gen, err)
			} else if reply.Abort {
				failed = true
				status = ""
				paste = 0
				c.showError(gen, errors.New("远端无法读取或写入此剪贴板内容"))
			} else if m.Part != nil || len(m.Data) > 0 || reply.Part != nil || len(reply.Data) > 0 {
				failed = false
				if !p.Busy() && status != "" {
					c.setStatus(gen, "远端"+status+"已就绪，可以粘贴")
					status = ""
				}
			}
		}
		if !p.Busy() && !abort {
			if failed {
				paste = 0
			}
			if paste != 0 && p.Current() {
				s.FinishPaste(paste)
				paste = 0
			}
			timer := time.NewTimer(400 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-s.Done():
				timer.Stop()
				return
			case paste = <-pastes:
				timer.Stop()
			case <-timer.C:
			}
		}
	}
}
func (c *Controller) showError(gen uint64, err error) {
	c.cx.Post(func() {
		if c.active(gen) {
			c.Status.Set(err.Error())
			c.report(err)
		}
	})
}

func (c *Controller) setStatus(gen uint64, status string) {
	c.cx.Post(func() {
		if c.active(gen) {
			c.Status.Set(status)
		}
	})
}
