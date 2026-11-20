//go:build !linux || android

package portal

import "context"

// Session is a portal session; there are none off desktop Linux.
type Session struct{}

func Prepare(bool)                               {}
func Current() *Session                          { return nil }
func Get(context.Context) (*Session, error)      { return nil, ErrUnavailable }
func (*Session) Streams() []Stream               { return nil }
func (*Session) Input() bool                     { return false }
func (*Session) Done() <-chan struct{}           { return nil }
func (*Session) PipeWire() (int, error)          { return -1, ErrUnavailable }
func (*Session) Motion(uint32, float64, float64) {}
func (*Session) Button(int32, bool)              {}
func (*Session) Axis(float64, float64)           {}
func (*Session) Key(int32, bool)                 {}
func (*Session) Keysym(int32, bool)              {}
