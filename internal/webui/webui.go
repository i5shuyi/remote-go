// Package webui holds the browser front end the host serves on the tailnet
// (remote-go view, or any browser on the tailnet):
//
//	static/viewer.js   remote-session client (WebCodecs H.264 / JPEG)
//	static/index.html  the page
//
// The desktop app draws its own native interface (package gui).
package webui

import (
	"embed"
	"io/fs"
)

//go:embed static
var static embed.FS

// HostFS is served by the host on the tailnet (browser access).
func HostFS() fs.FS {
	f, err := fs.Sub(static, "static")
	if err != nil {
		panic(err)
	}
	return f
}
