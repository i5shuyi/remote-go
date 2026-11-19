//go:build !windows

package liveupdate

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
)

// Keep the Darwin socket path below sockaddr_un's 104-byte limit, including
// when the account's home directory has a long name.
func socketDir() (string, error) {
	config, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256([]byte(config))
	dir := filepath.Join("/tmp", fmt.Sprintf("remote-go-%d-%x", os.Getuid(), hash[:8]))
	if err = os.Mkdir(dir, 0700); err != nil && !os.IsExist(err) {
		return "", err
	}
	st, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	if !st.IsDir() || st.Mode().Perm() != 0700 || st.Sys().(*syscall.Stat_t).Uid != uint32(os.Getuid()) {
		return "", fmt.Errorf("unsafe session socket directory %q", dir)
	}
	return dir, nil
}
func dialLocal(ctx context.Context) (net.Conn, error) {
	dir, err := socketDir()
	if err != nil {
		return nil, err
	}
	var d net.Dialer
	return d.DialContext(ctx, "unix", filepath.Join(dir, "control.sock"))
}
func listenLocal() (net.Listener, func(), error) {
	dir, err := socketDir()
	if err != nil {
		return nil, nil, err
	}
	lock, err := os.OpenFile(filepath.Join(dir, "lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, nil, err
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, nil, err
	}
	path := filepath.Join(dir, "control.sock")
	_ = os.Remove(path) // An earlier owner may have crashed while holding the lock.
	ln, err := net.Listen("unix", path)
	if err != nil {
		lock.Close()
		return nil, nil, err
	}
	return ln, func() { ln.Close(); lock.Close() }, nil
}
