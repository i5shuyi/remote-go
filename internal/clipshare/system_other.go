//go:build !android && !windows && !(darwin && cgo)

package clipshare

func System() Board { return nil }
