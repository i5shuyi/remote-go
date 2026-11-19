//go:build android

package clipshare

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"
)

type androidBoard struct{}
type bridgeReply struct {
	Version                uint64
	Text, ImagePath, Error string
	Files                  []string
}

func System() Board { return androidBoard{} }
func call(ctx context.Context, op string, args map[string]any) (bridgeReply, error) {
	var reply bridgeReply
	name, err := os.ReadFile(filepath.Join(os.Getenv("NSGO_CACHE_DIR"), "remote-go-clip.socket"))
	if err != nil {
		return reply, err
	}
	conn, err := (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "unix", "@"+string(name))
	if err != nil {
		return reply, err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	conn.SetDeadline(time.Now().Add(2 * time.Second))
	if op == "get" {
		conn.SetDeadline(time.Now().Add(3 * time.Minute))
	}
	if args == nil {
		args = map[string]any{}
	}
	args["op"] = op
	if err = json.NewEncoder(conn).Encode(args); err != nil {
		return reply, err
	}
	err = json.NewDecoder(io.LimitReader(conn, 2<<20)).Decode(&reply)
	if err == nil && reply.Error != "" {
		err = errors.New(reply.Error)
	}
	return reply, err
}
func (androidBoard) Version() uint64 {
	r, _ := call(context.Background(), "version", nil)
	return r.Version
}
func (androidBoard) Read() ([]byte, bool) {
	r, e := call(context.Background(), "text", nil)
	return []byte(r.Text), e == nil
}
func (androidBoard) Write(b []byte) bool {
	_, e := call(context.Background(), "setText", map[string]any{"text": string(b)})
	return e == nil
}
func (androidBoard) ReadRich(ctx context.Context) (Content, error) {
	root, err := cacheRoot()
	if err != nil {
		return Content{}, err
	}
	dir, err := os.MkdirTemp(root, "import-")
	if err != nil {
		return Content{}, err
	}
	cleanup := func() { os.RemoveAll(dir) }
	r, err := call(ctx, "get", map[string]any{"dir": dir})
	if err != nil {
		cleanup()
		return Content{}, err
	}
	if r.ImagePath != "" {
		defer cleanup()
		b, err := os.ReadFile(r.ImagePath)
		return Content{PNG: b}, err
	}
	if len(r.Files) == 0 {
		cleanup()
		return Content{}, nil
	}
	return Content{Files: r.Files, cleanup: cleanup}, nil
}
func (androidBoard) WriteRich(c Content) error {
	files := c.Files
	dir := ""
	if len(c.PNG) > 0 {
		root, err := cacheRoot()
		if err != nil {
			return err
		}
		dir, err = os.MkdirTemp(root, "batch-")
		if err != nil {
			return err
		}
		path := filepath.Join(dir, "clipboard.png")
		if err = os.WriteFile(path, c.PNG, 0600); err != nil {
			os.RemoveAll(dir)
			return err
		}
		files = []string{path}
	}
	_, err := call(context.Background(), "set", map[string]any{"image": dir != "", "files": files})
	if dir != "" {
		if err != nil {
			os.RemoveAll(dir)
		} else {
			sweep(files)
		}
	}
	return err
}
