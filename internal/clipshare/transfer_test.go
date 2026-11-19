package clipshare

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

type richTestBoard struct {
	testBoard
	item Content
}

func (b *richTestBoard) ReadRich(context.Context) (Content, error) { b.accesses++; return b.item, nil }
func (b *richTestBoard) WriteRich(c Content) error                 { b.item = c; b.ver++; b.writes++; return nil }

func TestRichClipboardSession(t *testing.T) {
	t.Setenv("NSGO_CACHE_DIR", t.TempDir())
	files := map[string][]byte{"0/相册.txt": bytes.Repeat([]byte("a"), ChunkSize+5), "1/相册.txt": []byte("second"), "2/空": {}}
	src := t.TempDir()
	names := []string{"0/相册.txt", "1/相册.txt", "2/空"}
	local, remote := &richTestBoard{}, &richTestBoard{}
	for _, name := range names {
		path := filepath.Join(src, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, files[name], 0600); err != nil {
			t.Fatal(err)
		}
		local.item.Files = append(local.item.Files, path)
	}
	var h Host
	ctx := context.Background()
	c := NewPeer(local)
	defer c.Close()
	defer h.Close()
	h.Handle(remote, Message{Generation: 1, Operation: "on", Rich: true})
	poll := func() Message {
		m := Message{Generation: 1, Operation: "poll"}
		if err := c.Next(ctx, &m); err != nil {
			t.Fatal(err)
		}
		r := h.Handle(remote, m)
		if r == nil || r.Abort {
			t.Fatal("transfer rejected")
		}
		if err := c.Apply(ctx, *r, true); err != nil {
			t.Fatal(err)
		}
		return m
	}
	if m := poll(); m.Part != nil {
		t.Fatal("sent old clipboard")
	}
	local.ver++
	if m := poll(); m.Part == nil || remote.writes != 0 {
		t.Fatal("published incomplete copy")
	}
	if m := poll(); m.Part == nil || remote.writes != 1 || len(remote.item.Files) != len(names) {
		t.Fatal("files not delivered")
	}
	for i, p := range remote.item.Files {
		b, err := os.ReadFile(p)
		if err != nil || filepath.Base(p) != filepath.Base(names[i]) || !bytes.Equal(b, files[names[i]]) || p == local.item.Files[i] {
			t.Fatalf("incorrect copy %q: %v", p, err)
		}
	}
	if m := poll(); m.Part != nil || local.writes != 0 || remote.writes != 1 {
		t.Fatal("echoed shared files")
	}
	img := image.NewNRGBA(image.Rect(0, 0, 2, 2))
	img.Set(1, 1, color.NRGBA{R: 190, A: 128})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	remote.item = Content{PNG: buf.Bytes()}
	remote.ver++
	poll()
	if local.writes != 1 || !bytes.Equal(local.item.PNG, buf.Bytes()) || len(local.item.Files) != 0 {
		t.Fatal("reverse image copy failed")
	}
	if m := poll(); m.Part != nil {
		t.Fatal("echoed shared image")
	}
}

func TestInterruptedAndInvalidClipboard(t *testing.T) {
	t.Setenv("NSGO_CACHE_DIR", t.TempDir())
	var h Host
	b := &richTestBoard{}
	part := &Part{Manifest: &Manifest{"files", []Entry{{"ok", 3}}}, Bytes: []byte{1}}
	poll := Message{Generation: 1, Operation: "poll", Part: part}
	if h.Handle(b, poll) != nil || b.accesses != 0 {
		t.Fatal("accepted before opt-in")
	}
	h.Handle(b, Message{Generation: 1, Operation: "on", Rich: true})
	if m := h.Handle(b, poll); m.Abort {
		t.Fatal("rejected start")
	}
	dir := h.peer.in.dir
	h.Handle(b, Message{Generation: 1, Operation: "off"})
	if _, err := os.Stat(dir); !os.IsNotExist(err) || b.writes != 0 {
		t.Fatal("partial files survived switch-off")
	}
	if h.Handle(b, poll) != nil {
		t.Fatal("accepted after switch-off")
	}
	for _, entry := range []Entry{{"..", 1}, {"../bad", 1}, {"C:\\bad", 1}, {"bad", -1}, {"too large", MaxTotal + 1}} {
		var r Receiver
		if _, _, err := r.Accept(&Part{Manifest: &Manifest{"files", []Entry{entry}}}); err == nil {
			t.Fatalf("accepted %q", entry.Name)
		}
	}
	h.Handle(b, Message{Generation: 2, Operation: "on", Rich: true})
	poll.Generation = 2
	h.Handle(b, poll)
	poll.Part = &Part{Offset: 2, Bytes: []byte{2}}
	if m := h.Handle(b, poll); !m.Abort {
		t.Fatal("accepted incorrect offset")
	}
	poll.Part = part
	h.Handle(b, poll)
	b.ver++
	poll.Part = &Part{Offset: 1, Bytes: []byte{2, 3}}
	if m := h.Handle(b, poll); !m.Abort || b.writes != 0 {
		t.Fatal("overwrote newer clipboard")
	}
	poll.Part = part
	h.Handle(b, poll)
	dir = h.peer.in.dir
	h.Close()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("partial files survived disconnect")
	}
}
