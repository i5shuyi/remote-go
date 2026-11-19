// Peer is owned by one session goroutine. Each poll moves at most one chunk.
package clipshare

import (
	"bytes"
	"context"
	"errors"
	"image/png"
)

var errBusy = errors.New("系统剪贴板正忙")

type Peer struct {
	board     RichBoard
	version   uint64
	inVersion uint64
	out       *Sender
	in        Receiver
}

func NewPeer(b RichBoard) *Peer { return &Peer{board: b, version: b.Version()} }
func (p *Peer) Close() {
	if p.out != nil {
		p.out.Close()
		p.out = nil
	}
	p.in.Close()
}
func (p *Peer) Busy() bool { return p.out != nil || p.in.meta != nil }

func (p *Peer) Next(ctx context.Context, m *Message) error {
	if p.in.meta != nil {
		return nil
	}
	if p.out == nil {
		v := p.board.Version()
		if v == p.version {
			return nil
		}
		c, err := p.board.ReadRich(ctx)
		if c.cleanup != nil {
			defer c.cleanup()
		}
		if errors.Is(err, errBusy) {
			return nil
		}
		if err != nil {
			p.version = v
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if c.Empty() {
			if data, ok := p.board.Read(); ok && p.board.Version() == v {
				if Valid(data) {
					m.Data = data
				}
				p.version = v
			}
			return nil
		}
		if p.board.Version() != v {
			return nil
		}
		p.version = v
		p.out, err = NewSender(c)
		if err != nil {
			return err
		}
	}
	part, done, err := p.out.Next()
	if err != nil {
		p.Close()
		return err
	}
	m.Part = part
	if done {
		p.out = nil
	}
	return nil
}

// Apply publishes only complete items. A newer local copy during reception wins.
func (p *Peer) Apply(ctx context.Context, m Message, preferLocal bool) error {
	if m.Abort {
		p.Close()
		return nil
	}
	if m.Part == nil && len(m.Data) == 0 {
		return nil
	}
	if preferLocal && p.board.Version() != p.version {
		p.Close()
		return errors.New("本机剪贴板已更新")
	}
	if p.out != nil {
		p.out.Close()
		p.out = nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if m.Part == nil {
		p.in.Close()
		if !Valid(m.Data) || !p.board.Write(m.Data) {
			return errors.New("无法写入剪贴板文本")
		}
		p.version = p.board.Version()
		return nil
	}
	if m.Part.Manifest != nil {
		p.inVersion = p.board.Version()
	}
	item, done, err := p.in.Accept(m.Part)
	if err != nil || !done {
		return err
	}
	defer p.in.Close()
	if len(item.PNG) > 0 {
		if _, err = png.Decode(bytes.NewReader(item.PNG)); err != nil {
			return errors.New("剪贴板 PNG 数据损坏")
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if p.board.Version() != p.inVersion {
		return errors.New("接收期间本机剪贴板已更新")
	}
	if err = p.board.WriteRich(item); err != nil {
		return err
	}
	p.version = p.board.Version()
	p.in.Published()
	if len(item.Files) > 0 {
		sweep(item.Files)
	}
	return nil
}

func (p *Peer) Current() bool { return p.board.Version() == p.version }
