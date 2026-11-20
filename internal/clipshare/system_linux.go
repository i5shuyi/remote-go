//go:build linux && !android

package clipshare

// The Linux board is the X11 CLIPBOARD selection, text only: the host
// serves UTF8_STRING itself while it owns the selection, and XFIXES tells
// it when another client takes it. Sessions with a richer peer fall back
// to plain text, as with older hosts.

import (
	"log"
	"sync"
	"time"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xfixes"
	"github.com/jezek/xgb/xproto"
)

type xBoard struct {
	c   *xgb.Conn
	win xproto.Window

	clipboard, utf8, targets, text, str, prop xproto.Atom

	mu      sync.Mutex
	version uint64
	owned   []byte                           // what we serve while we own CLIPBOARD
	reply   chan xproto.SelectionNotifyEvent // the pending Read's answer
	readMu  sync.Mutex                       // one Read at a time
}

var (
	boardOnce sync.Once
	board     *xBoard
)

func System() Board {
	boardOnce.Do(func() {
		b, err := newXBoard()
		if err != nil {
			log.Printf("剪贴板不可用：%v", err)
			return
		}
		board = b
	})
	if board == nil {
		return nil
	}
	return board
}

func newXBoard() (*xBoard, error) {
	c, err := xgb.NewConn()
	if err != nil {
		return nil, err
	}
	b := &xBoard{c: c, version: 1}
	fail := func(err error) (*xBoard, error) { c.Close(); return nil, err }
	screen := xproto.Setup(c).DefaultScreen(c)
	if b.win, err = xproto.NewWindowId(c); err != nil {
		return fail(err)
	}
	if err = xproto.CreateWindowChecked(c, 0, b.win, screen.Root, 0, 0, 1, 1, 0,
		xproto.WindowClassInputOnly, 0, 0, nil).Check(); err != nil {
		return fail(err)
	}
	for _, a := range []struct {
		name string
		atom *xproto.Atom
	}{{"CLIPBOARD", &b.clipboard}, {"UTF8_STRING", &b.utf8}, {"TARGETS", &b.targets},
		{"TEXT", &b.text}, {"STRING", &b.str}, {"REMOTE_GO_CLIP", &b.prop}} {
		r, err := xproto.InternAtom(c, false, uint16(len(a.name)), a.name).Reply()
		if err != nil {
			return fail(err)
		}
		*a.atom = r.Atom
	}
	// Without XFIXES the version only moves on our own writes.
	if xfixes.Init(c) == nil {
		if _, err := xfixes.QueryVersion(c, 5, 0).Reply(); err == nil {
			xfixes.SelectSelectionInput(c, b.win, b.clipboard, xfixes.SelectionEventMaskSetSelectionOwner|
				xfixes.SelectionEventMaskSelectionWindowDestroy|xfixes.SelectionEventMaskSelectionClientClose)
		}
	}
	go b.events()
	return b, nil
}

func (b *xBoard) events() {
	for {
		ev, err := b.c.WaitForEvent()
		if ev == nil && err == nil {
			return // connection closed
		}
		switch e := ev.(type) {
		case xfixes.SelectionNotifyEvent:
			b.mu.Lock()
			if e.Owner != b.win {
				b.version++
			}
			b.mu.Unlock()
		case xproto.SelectionClearEvent:
			b.mu.Lock()
			b.owned = nil
			b.mu.Unlock()
		case xproto.SelectionRequestEvent:
			b.serve(e)
		case xproto.SelectionNotifyEvent:
			b.mu.Lock()
			ch := b.reply
			b.mu.Unlock()
			if ch != nil {
				select {
				case ch <- e:
				default:
				}
			}
		}
	}
}

// serve answers another client's request for what we own.
func (b *xBoard) serve(e xproto.SelectionRequestEvent) {
	b.mu.Lock()
	data := b.owned
	b.mu.Unlock()
	prop := e.Property
	if prop == 0 {
		prop = e.Target // obsolete clients
	}
	switch {
	case data == nil || e.Selection != b.clipboard:
		prop = 0
	case e.Target == b.targets:
		atoms := []xproto.Atom{b.targets, b.utf8, b.text, b.str}
		buf := make([]byte, 4*len(atoms))
		for i, a := range atoms {
			xgb.Put32(buf[4*i:], uint32(a))
		}
		xproto.ChangeProperty(b.c, xproto.PropModeReplace, e.Requestor, prop, xproto.AtomAtom, 32, uint32(len(atoms)), buf)
	case e.Target == b.utf8 || e.Target == b.text || e.Target == b.str:
		xproto.ChangeProperty(b.c, xproto.PropModeReplace, e.Requestor, prop, e.Target, 8, uint32(len(data)), data)
	default:
		prop = 0
	}
	ev := xproto.SelectionNotifyEvent{Time: e.Time, Requestor: e.Requestor, Selection: e.Selection, Target: e.Target, Property: prop}
	xproto.SendEvent(b.c, false, e.Requestor, 0, string(ev.Bytes()))
}

func (b *xBoard) Version() uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.version
}

func (b *xBoard) Read() ([]byte, bool) {
	b.mu.Lock()
	if b.owned != nil {
		data := append([]byte(nil), b.owned...)
		b.mu.Unlock()
		return data, true
	}
	b.mu.Unlock()
	owner, err := xproto.GetSelectionOwner(b.c, b.clipboard).Reply()
	if err != nil {
		return nil, false
	}
	if owner.Owner == 0 {
		return nil, true // empty clipboard
	}
	b.readMu.Lock()
	defer b.readMu.Unlock()
	ch := make(chan xproto.SelectionNotifyEvent, 1)
	b.mu.Lock()
	b.reply = ch
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		b.reply = nil
		b.mu.Unlock()
	}()
	xproto.ConvertSelection(b.c, b.win, b.clipboard, b.utf8, b.prop, xproto.TimeCurrentTime)
	select {
	case e := <-ch:
		if e.Property == 0 {
			return nil, true // the owner has no text
		}
	case <-time.After(time.Second):
		return nil, false
	}
	r, err := xproto.GetProperty(b.c, true, b.win, b.prop, xproto.GetPropertyTypeAny, 0, (MaxBytes+3)/4+1).Reply()
	if err != nil {
		return nil, false
	}
	if r.BytesAfter > 0 || r.Format != 8 {
		return nil, true // oversized (or INCR) or not text
	}
	return r.Value, true
}

func (b *xBoard) Write(data []byte) bool {
	b.mu.Lock()
	b.owned = append([]byte{}, data...)
	b.version++
	b.mu.Unlock()
	xproto.SetSelectionOwner(b.c, b.win, b.clipboard, xproto.TimeCurrentTime)
	r, err := xproto.GetSelectionOwner(b.c, b.clipboard).Reply()
	return err == nil && r.Owner == b.win
}
