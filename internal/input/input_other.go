//go:build !windows && !(darwin && cgo) && !(linux && !android)

package input

func newInjector() (Injector, error) { return nil, ErrUnsupported }
