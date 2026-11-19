package clipshare

import (
	"bytes"
	"testing"
)

type testBoard struct {
	text             string
	ver              uint64
	accesses, writes int
}

func (b *testBoard) Version() uint64      { b.accesses++; return b.ver }
func (b *testBoard) Read() ([]byte, bool) { b.accesses++; return []byte(b.text), true }
func (b *testBoard) Write(p []byte) bool {
	b.accesses++
	b.writes++
	b.text = string(p)
	b.ver++
	return true
}

func TestSessionConsentAndChanges(t *testing.T) {
	b := &testBoard{text: "copied before connecting"}
	var host, other Host
	call := func(gen uint64, op, text string) *Message {
		return host.Handle(b, Message{Generation: gen, Operation: op, Data: []byte(text)})
	}
	if call(1, "poll", "unsolicited") != nil || b.accesses != 0 {
		t.Fatal("access before opt-in")
	}
	call(1, "on", "")
	if r := call(1, "poll", ""); r == nil || len(r.Data) != 0 {
		t.Fatal("shared pre-existing contents")
	}
	b.Write([]byte("远端复制\n🌳"))
	if r := call(1, "poll", ""); r == nil || string(r.Data) != b.text {
		t.Fatal("missing host copy")
	}
	if r := call(1, "poll", ""); r == nil || len(r.Data) != 0 {
		t.Fatal("repeated unchanged copy")
	}
	call(1, "poll", "本机复制")
	if b.text != "本机复制" {
		t.Fatal("local copy not applied")
	}
	if r := call(1, "poll", ""); r == nil || len(r.Data) != 0 {
		t.Fatal("echoed a synchronized copy")
	}
	n := b.accesses
	if r := other.Handle(b, Message{Generation: 1, Operation: "poll"}); r != nil || b.accesses != n {
		t.Fatal("shared with another session")
	}
	call(1, "off", "")
	if call(1, "poll", "late") != nil || b.accesses != n {
		t.Fatal("access after disabling")
	}
	call(2, "on", "")
	n = b.accesses
	if call(1, "poll", "stale") != nil || b.accesses != n {
		t.Fatal("stale generation accessed board")
	}
	for _, data := range [][]byte{bytes.Repeat([]byte("x"), MaxBytes+1), {0xff}, {'a', 0, 'b'}} {
		host.Handle(b, Message{Generation: 2, Operation: "poll", Data: data})
	}
	if b.writes != 2 {
		t.Fatalf("unexpected clipboard writes: %d", b.writes)
	}
}
