//go:build !darwin || !cgo

package gui

func watchUpdateRestart()    {}
func updateRestarting() bool { return false }
