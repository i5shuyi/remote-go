// Package clipshare exchanges new clipboard contents within an opted-in session.
package clipshare

import (
	"bytes"
	"context"
	"unicode/utf8"
)

// MaxBytes is the plain-text limit used by both protocol versions.
const MaxBytes = 32 << 10

type Message struct {
	Generation uint64 `json:"clipID,omitempty"`
	Operation  string `json:"clipOp,omitempty"`
	Rich       bool   `json:"clipRich,omitempty"`
	Part       *Part  `json:"clipPart,omitempty"`
	Abort      bool   `json:"clipAbort,omitempty"`
	Data       []byte `json:"clipData,omitempty"`
}

func (m Message) Packet() any {
	return struct {
		T string `json:"t"`
		Message
	}{"clip", m}
}

func Valid(data []byte) bool {
	return len(data) > 0 && len(data) <= MaxBytes && utf8.Valid(data) && bytes.IndexByte(data, 0) < 0
}

type Board interface {
	Version() uint64
	// A successful read may be empty for non-text or oversized data.
	Read() ([]byte, bool)
	Write([]byte) bool
}

// Host belongs to a single authenticated connection, on its read goroutine.
type Host struct {
	generation uint64
	peer       *Peer
	version    uint64
}

func (h *Host) Close() {
	if h.peer != nil {
		h.peer.Close()
	}
	*h = Host{}
}
func (h *Host) Handle(b Board, m Message) *Message {
	return h.HandleContext(context.Background(), b, m)
}
func (h *Host) HandleContext(ctx context.Context, b Board, m Message) *Message {
	if b == nil || m.Generation == 0 {
		return nil
	}
	if m.Operation == "on" {
		h.Close()
		if rb, ok := b.(RichBoard); ok && m.Rich {
			h.peer = NewPeer(rb)
		}
		h.generation, h.version = m.Generation, b.Version()
		return nil
	}
	if m.Generation != h.generation {
		return nil
	}
	switch m.Operation {
	case "off":
		h.Close()
	case "poll":
		out := Message{Generation: m.Generation, Operation: "result"}
		if h.peer != nil {
			var err error
			if m.Part != nil || len(m.Data) > 0 || m.Abort {
				err = h.peer.Apply(ctx, m, false)
			} else {
				err = h.peer.Next(ctx, &out)
			}
			if err != nil {
				h.peer.Close()
				out.Abort = true
			}
			return &out
		}
		if len(m.Data) > 0 {
			if Valid(m.Data) {
				current, ok := b.Read()
				if (!ok || !bytes.Equal(current, m.Data)) && !b.Write(m.Data) {
					out.Operation = "retry"
					return &out
				}
				h.version = b.Version()
			}
		} else if v := b.Version(); v != h.version {
			if data, ok := b.Read(); ok {
				if Valid(data) {
					out.Data = data
				}
				h.version = v
			}
		}
		return &out
	}
	return nil
}
