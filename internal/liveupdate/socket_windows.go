package liveupdate

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"

	"github.com/tailscale/go-winio"
	"golang.org/x/sys/windows"
)

func pipeAddress() (string, *windows.SID, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", nil, err
	}
	var session uint32
	if err = windows.ProcessIdToSessionId(uint32(os.Getpid()), &session); err != nil {
		return "", nil, err
	}
	sid := user.User.Sid
	return fmt.Sprintf(`\\.\pipe\remote-go-session-v1-%s-%d`, sid, session), sid, nil
}
func listenLocal() (net.Listener, func(), error) {
	path, sid, err := pipeAddress()
	if err != nil {
		return nil, nil, err
	}
	ln, err := winio.ListenPipe(path, &winio.PipeConfig{SecurityDescriptor: "O:" + sid.String() + "D:P(A;;GA;;;" + sid.String() + ")"})
	if err != nil {
		return nil, nil, err
	}
	return ln, func() { ln.Close() }, nil
}
func dialLocal(ctx context.Context) (net.Conn, error) {
	path, sid, err := pipeAddress()
	if err != nil {
		return nil, err
	}
	c, err := winio.DialPipeAccess(ctx, path, windows.GENERIC_READ|windows.GENERIC_WRITE|windows.READ_CONTROL)
	if err != nil {
		return nil, err
	}
	fd := c.(interface{ Fd() uintptr }).Fd()
	sd, err := windows.GetSecurityInfo(windows.Handle(fd), windows.SE_KERNEL_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err == nil {
		owner, _, e := sd.Owner()
		if e == nil && sid.Equals(owner) {
			return c, nil
		}
		err = errors.New("session pipe owner differs from the current user")
	}
	c.Close()
	return nil, err
}
