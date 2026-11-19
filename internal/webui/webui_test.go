package webui

import (
	"io/fs"
	"testing"
)

func TestFS(t *testing.T) {
	for _, n := range []string{"index.html", "viewer.js"} {
		if _, err := fs.Stat(HostFS(), n); err != nil {
			t.Errorf("HostFS missing %s: %v", n, err)
		}
	}
}
