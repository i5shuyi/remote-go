package members

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

// Limits on what a peer can make us read before we have verified it.
const (
	maxFrame      = 1 << 20
	maxUnverified = 8 << 20
	handshakeTime = 30 * time.Second
)

type frames struct {
	r      io.Reader
	w      io.Writer
	budget int
}

func newFrames(r io.Reader, w io.Writer) *frames { return &frames{r: r, w: w, budget: maxUnverified} }

func (f *frames) send(v any) error {
	b, err := marshalWire(v)
	if err != nil {
		return err
	}
	if len(b) > maxFrame {
		return errors.New("消息过大")
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(b)))
	if _, err := f.w.Write(append(hdr[:], b...)); err != nil {
		return err
	}
	return nil
}

func (f *frames) recv(v any) error {
	var hdr [4]byte
	if _, err := io.ReadFull(f.r, hdr[:]); err != nil {
		return err
	}
	n := int(binary.BigEndian.Uint32(hdr[:]))
	if n > maxFrame || n > f.budget {
		return fmt.Errorf("消息过大（%d 字节）", n)
	}
	f.budget -= n
	b := make([]byte, n)
	if _, err := io.ReadFull(f.r, b); err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// exchange sends out while receiving in, so both peers can speak first.
func (f *frames) exchange(out, in any) error {
	errc := make(chan error, 1)
	go func() { errc <- f.send(out) }()
	rerr := f.recv(in)
	serr := <-errc
	if serr != nil {
		return serr
	}
	return rerr
}

func deadline(c net.Conn, d time.Duration) {
	if d == 0 {
		c.SetDeadline(time.Time{})
	} else {
		c.SetDeadline(time.Now().Add(d))
	}
}
