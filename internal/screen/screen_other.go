//go:build !darwin && !(windows && (amd64 || arm64))

package screen

import "github.com/nsgo-dev/nsgo/core"

func newView(cx *core.NativeContext) core.NativeComponent { return nil }
