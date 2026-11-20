//go:build !darwin && !(windows && (amd64 || arm64)) && !(linux && !android)

package screen

import "github.com/nsgo-dev/nsgo/core"

func newView(cx *core.NativeContext) core.NativeComponent { return nil }
