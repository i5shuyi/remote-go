//go:build darwin && cgo

package gui

/*
#cgo LDFLAGS: -framework Foundation
void rg_watch_update_restart(void);
int rg_update_restarting(void);
*/
import "C"

// Sparkle posts this before asking NSApplication to terminate. A normal
// Cmd+Q and an update may otherwise arrive through the same quit handler.
func watchUpdateRestart()    { C.rg_watch_update_restart() }
func updateRestarting() bool { return C.rg_update_restarting() != 0 }
