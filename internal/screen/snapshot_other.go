//go:build !darwin

package screen

// Snapshot is only implemented on macOS.
func Snapshot(dir string) {}
