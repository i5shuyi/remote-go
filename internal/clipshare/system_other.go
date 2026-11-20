//go:build !linux && !windows && !(darwin && cgo)

package clipshare

func System() Board { return nil }
