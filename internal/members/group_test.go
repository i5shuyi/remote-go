package members

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestCanonicalize(t *testing.T) {
	for in, want := range map[string]string{
		`{"b":1, "a":[true,null,"x"]}`: `{"a":[true,null,"x"],"b":1}`,
		`{"é":1,"z":2,"😀":3}`:          `{"z":2,"é":1,"😀":3}`, // UTF-16 order
		`"\u0001\n <"`:                 `"\u0001\n` + " " + `<"`,
	} {
		got, err := canonicalize([]byte(in))
		if err != nil || string(got) != want {
			t.Errorf("%s → %s, %v; want %s", in, got, err, want)
		}
	}
	for _, bad := range []string{`{"a":1,"a":2}`, `{"a":1.5}`, `{"a":1e3}`, `{"a":9007199254740992}`, `{} {}`, "\"\xff\""} {
		if _, err := canonicalize([]byte(bad)); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestDecodeStrict(t *testing.T) {
	var a Ack
	good := `{"group":"` + strings.Repeat("A", 43) + `","head":"` + strings.Repeat("A", 43) + `","key_version":"` + strings.Repeat("A", 43) + `","member":"` + strings.Repeat("A", 43) + `","v":1}`
	if err := decodeStrict([]byte(good), &a); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{
		strings.Replace(good, `"v":1`, `"v":1,"x":1`, 1),                                              // unknown field
		strings.Replace(good, `,"v":1`, ``, 1),                                                        // missing field
		strings.Replace(good, `"head"`, ` "head"`, 1),                                                 // not canonical
		strings.Replace(good, strings.Repeat("A", 43)+`","key`, strings.Repeat("A", 42)+`B","key`, 1), // non-zero trailing bits
	} {
		if err := decodeStrict([]byte(bad), &a); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	var s Signed
	if err := json.Unmarshal([]byte(`{"type":"nope","body":{},"sig":""}`), &s); err == nil {
		t.Error("accepted unknown type")
	}
}

type world struct {
	g  *Group
	id map[string]Identity
}

// newWorld makes a group of the named members, all added by the first.
func newWorld(t *testing.T, names ...string) *world {
	t.Helper()
	w := &world{id: map[string]Identity{}}
	for _, n := range names {
		w.id[n] = NewIdentity()
	}
	g, err := Create(w.id[names[0]], NewGroupKey())
	if err != nil {
		t.Fatal(err)
	}
	w.g = g
	for _, n := range names[1:] {
		w.add(t, names[0], n)
	}
	return w
}

func (w *world) add(t *testing.T, inviter, joiner string) Signed {
	t.Helper()
	in, _ := NewIntent(w.id[joiner], w.g.ID(), w.id[inviter].ID(), ID{1})
	r, err := w.g.Add(w.id[inviter], in)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestHistoryRules(t *testing.T) {
	w := newWorld(t, "A", "B", "C")
	if n := len(w.g.State().Members); n != 3 {
		t.Fatalf("%d members", n)
	}
	// Same identity joins once.
	in, _ := NewIntent(w.id["B"], w.g.ID(), w.id["A"].ID(), ID{2})
	if _, err := w.g.Add(w.id["A"], in); err == nil {
		t.Fatal("re-added B")
	}
	// Intent for another inviter does not fit.
	d := NewIdentity()
	in, _ = NewIntent(d, w.g.ID(), w.id["C"].ID(), ID{3})
	if _, err := w.g.Add(w.id["A"], in); err == nil {
		t.Fatal("accepted mismatched intent")
	}

	// Replica that receives the history by sync.
	replica, err := Load(w.g.Genesis(), nil)
	if err != nil {
		t.Fatal(err)
	}
	h := w.g.History()
	if _, err := replica.Apply(h[1:]); !errors.Is(err, ErrUnknownParent) {
		t.Fatalf("missing prefix: %v", err)
	}
	if r, err := replica.Apply(h); err != nil || r.Added != 2 {
		t.Fatal(r, err)
	}
	if r, err := replica.Apply(h); err != nil || r.Added != 0 {
		t.Fatal("dedupe", r, err)
	}

	// A removes B; an add B signs afterwards is invalid and is not a fork.
	rm, res, err := w.g.Remove(w.id["A"], w.id["B"].ID(), nil)
	if err != nil || len(res.Removed) != 1 {
		t.Fatal(res, err)
	}
	e := NewIdentity()
	in, _ = NewIntent(e, w.g.ID(), w.id["B"].ID(), ID{4})
	bad, _ := sign(w.id["B"].Priv, TypeChange, Change{V: Version, Group: w.g.ID(), Parent: rm.ID(), Issuer: w.id["B"].ID(), Op: OpAdd,
		Add: &AddParams{Key: B64(e.Public()), Member: e.ID(), Intent: in}})
	if _, err := w.g.Apply([]Signed{bad}); err == nil || errors.Is(err, ErrFork) {
		t.Fatalf("removed issuer: %v", err)
	}
	// Tampered signature rejects the message and changes nothing.
	head := w.g.Head()
	forged := w.g.History()[0]
	forged.Sig = append(B64{}, forged.Sig...)
	forged.Sig[0] ^= 1
	if _, err := replica.Apply([]Signed{forged}); err == nil {
		t.Fatal("accepted forged signature")
	}
	if w.g.Head() != head {
		t.Fatal("head moved")
	}
	if got := w.g.Invited(w.id["A"].ID()); len(got) != 1 {
		t.Fatalf("invited by A: %v", got)
	}
}

func TestForkDetection(t *testing.T) {
	w := newWorld(t, "A", "B")
	other, _ := Load(w.g.Genesis(), w.g.History())
	ra, err := w.g.Rotate(w.id["A"], NewGroupKey())
	if err != nil {
		t.Fatal(err)
	}
	rb, err := other.Rotate(w.id["B"], NewGroupKey())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.g.Apply(other.Suffix(ID{})); !errors.Is(err, ErrFork) {
		t.Fatalf("want fork, got %v", err)
	}
	c := w.g.Conflict()
	if c == nil || !c.Ours.Equal(ra) || !c.Theirs[0].Equal(rb) {
		t.Fatal("bad evidence")
	}
	if _, err := w.g.Rotate(w.id["A"], NewGroupKey()); !errors.Is(err, ErrConflict) {
		t.Fatal("ops must stop in conflict")
	}
	if p := w.g.ConflictPeers(); len(p) != 2 {
		t.Fatalf("peers %v", p)
	}
	// A third device learns the conflict from relayed evidence.
	third, _ := Load(w.g.Genesis(), w.g.History()[:1])
	if err := third.ApplyConflict(*c); !errors.Is(err, ErrFork) || third.Conflict() == nil {
		t.Fatalf("relay: %v", err)
	}
	// Garbage head is not evidence.
	fake := *c
	fake.Theirs = []Signed{c.Ours}
	fresh, _ := Load(w.g.Genesis(), w.g.History()[:1])
	if err := fresh.ApplyConflict(fake); err == nil || fresh.Conflict() != nil {
		t.Fatal("accepted identical evidence")
	}
}

func TestProfilesAcksReceipts(t *testing.T) {
	w := newWorld(t, "A", "B")
	A, B := w.id["A"], w.id["B"]
	relay := Relay{Region: 304}
	p1, err := w.g.NewProfile(B, "Mac", "darwin", true, []Relay{relay})
	if err != nil {
		t.Fatal(err)
	}
	p2, _ := w.g.NewProfile(B, "Mac mini", "darwin", true, []Relay{relay})
	if ok, _ := w.g.ApplyProfile(p1); ok {
		t.Fatal("older profile replaced newer")
	}
	if p, _ := w.g.Profile(B.ID()); p.Name != "Mac mini" || p.Seq != "1" {
		t.Fatalf("%+v", p)
	}
	_ = p2
	ack, _ := w.g.NewAck(A, w.g.State().KeyVersion)
	if _, err := w.g.ApplyAck(ack); err != nil {
		t.Fatal(err)
	}
	rm, _, _ := w.g.Remove(A, B.ID(), NewGroupKey())
	conf, pend := w.g.Progress(A.ID(), rm.ID())
	if len(conf) != 1 || len(pend) != 0 { // B is gone; only A remains
		t.Fatal(conf, pend)
	}
	if w.g.KeySignedOff(A.ID(), w.g.State().KeyVersion) != true {
		t.Fatal("only self remains: signed off")
	}
	if _, ok := w.g.Profile(B.ID()); ok {
		t.Fatal("removed member's profile kept")
	}

	w2 := newWorld(t, "A", "B", "C")
	w2.g.NewProfile(w2.id["B"], "B", "linux", true, []Relay{{Region: 302}, {Region: 301}})
	if w2.g.MigrationDone(w2.id["B"].ID(), Relay{Region: 302}) {
		t.Fatal("done without receipts")
	}
	w2.g.NewReceipt(w2.id["A"], w2.id["B"].ID())
	w2.g.NewReceipt(w2.id["C"], w2.id["B"].ID())
	if !w2.g.MigrationDone(w2.id["B"].ID(), Relay{Region: 302}) {
		t.Fatal("receipts from all members")
	}
}
