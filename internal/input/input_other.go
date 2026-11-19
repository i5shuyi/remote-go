//go:build !windows && !(darwin && cgo)

package input

func newInjector() (Injector, error) { return nil, ErrUnsupported }
