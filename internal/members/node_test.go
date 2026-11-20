package members

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"
)

type dev struct {
	*Node
	dir  string
	keys *MemKeyStore
	name string
}

func newDev(t *testing.T, name string) *dev {
	t.Helper()
	d := &dev{dir: t.TempDir(), keys: &MemKeyStore{}, name: name}
	st, err := OpenStore(d.dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	n, err := OpenNode(st, d.keys)
	if err != nil {
		t.Fatal(err)
	}
	d.Node = n
	return d
}

func (d *dev) id() ID { s, _ := d.Self(); return s.ID() }

type pairOut struct {
	codes      [2]string
	dial, serv error
}

// pair runs the pairing protocol; dialer is the controller side.
func pair(t *testing.T, dialer, desktop *dev, confirm [2]bool, inbound bool) pairOut {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tokens := &TokenBook{}
	tok := tokens.Issue("session-1")
	c1, c2 := net.Pipe()
	var out pairOut
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		out.serv = desktop.PairServe(ctx, c2, tokens, PairOptions{Name: desktop.name, Platform: "darwin", CanInbound: true,
			Confirm: func(_ context.Context, p PairPrompt) (bool, bool) { out.codes[1] = p.Code; return confirm[1], inbound }})
	}()
	out.dial = dialer.PairDial(ctx, c1, tok, PairOptions{Name: dialer.name, Platform: "android", CanInbound: inbound,
		Confirm: func(_ context.Context, p PairPrompt) (bool, bool) { out.codes[0] = p.Code; return confirm[0], true }})
	wg.Wait()
	if _, ok := tokens.Redeem(tok); ok {
		t.Fatal("token reusable")
	}
	return out
}

func TestPairBothDirections(t *testing.T) {
	a, b, c := newDev(t, "A"), newDev(t, "B"), newDev(t, "C")
	// Neither in a group: the controller creates it and invites the desktop.
	o := pair(t, a, b, [2]bool{true, true}, true)
	if o.dial != nil || o.serv != nil {
		t.Fatal(o.dial, o.serv)
	}
	if o.codes[0] == "" || o.codes[0] != o.codes[1] || len(o.codes[0]) != 19 {
		t.Fatalf("codes %q", o.codes)
	}
	// The desktop invites a new phone that dials it.
	if o := pair(t, c, b, [2]bool{true, true}, false); o.dial != nil || o.serv != nil {
		t.Fatal(o.dial, o.serv)
	}
	for _, d := range []*dev{a, b, c} {
		s := d.Snapshot()
		if s.Err != nil || !s.InGroup {
			t.Fatalf("%s: %+v", d.name, s)
		}
	}
	if b.Snapshot().Group != a.Snapshot().Group || len(b.Snapshot().Members) != 3 {
		t.Fatal("group mismatch")
	}
	if !b.Policy().AllowInbound || c.Policy().AllowInbound {
		t.Fatal("inbound policy")
	}
	// The inviter received the joiner's signed ack.
	if m := memberRow(b.Snapshot(), c.id()); !m.Synced {
		t.Fatalf("C not confirmed at B: %+v", m)
	}
	// Two devices of different groups are refused.
	x, y := newDev(t, "X"), newDev(t, "Y")
	if o := pair(t, x, y, [2]bool{true, true}, false); o.dial != nil {
		t.Fatal(o.dial)
	}
	if o := pair(t, x, a, [2]bool{true, true}, false); o.dial == nil {
		t.Fatal("paired across groups")
	}
}

func memberRow(s Snapshot, id ID) MemberInfo {
	for _, m := range s.Members {
		if m.ID == id {
			return m
		}
	}
	return MemberInfo{}
}

func TestPairDeclined(t *testing.T) {
	a, b := newDev(t, "A"), newDev(t, "B")
	o := pair(t, a, b, [2]bool{true, false}, true)
	if o.dial == nil || o.serv == nil {
		t.Fatal("pairing must fail")
	}
	if a.Snapshot().InGroup || b.Snapshot().InGroup {
		t.Fatal("authorization added")
	}
	for _, d := range []*dev{a, b} {
		slots, _ := d.keys.List()
		for _, s := range slots {
			if s[:2] == "k/" {
				t.Fatalf("%s holds key slot %s", d.name, s)
			}
		}
	}
}

func TestPairRetryIdempotent(t *testing.T) {
	a := newDev(t, "A")
	if _, err := a.CreateGroup(); err != nil {
		t.Fatal(err)
	}
	me, _ := a.Self()
	j := NewIdentity()
	txn := ID{9}
	in, _ := NewIntent(j, a.Snapshot().Group, me.ID(), txn)
	d1 := a.addForPairing(me, in, txn, j.ID())
	d2 := a.addForPairing(me, in, txn, j.ID())
	if d1.Error != "" || d2.Error != "" || len(d2.History) != 1 {
		t.Fatalf("%q %q %d", d1.Error, d2.Error, len(d2.History))
	}
	if a.chooseTxn(txn, j.ID()) != txn || a.chooseTxn(txn, ID{1}) == txn {
		t.Fatal("txn reuse")
	}
	// A removed identity retrying is refused.
	a.Remove(j.ID(), false)
	if d := a.addForPairing(me, in, txn, j.ID()); d.Error == "" {
		t.Fatal("delivered to removed member")
	}
}

// serve runs d's member handler on a local TLS listener.
func serve(t *testing.T, d *dev, control ControlHandler) string {
	t.Helper()
	return serveHandler(t, d, d.Handler(control))
}

func serveHandler(t *testing.T, d *dev, handler http.Handler) string {
	t.Helper()
	cfg, err := d.ServerTLS()
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: handler}
	go srv.Serve(tls.NewListener(ln, cfg))
	t.Cleanup(func() { srv.Close() })
	return ln.Addr().String()
}

func dialSync(t *testing.T, from *dev, addr string, to ID) (SyncResult, error) {
	t.Helper()
	raw, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return from.DialSync(ctx, raw, to)
}

func group3(t *testing.T) (a, b, c *dev) {
	a, b, c = newDev(t, "A"), newDev(t, "B"), newDev(t, "C")
	if o := pair(t, a, b, [2]bool{true, true}, true); o.dial != nil {
		t.Fatal(o.dial)
	}
	if o := pair(t, c, b, [2]bool{true, true}, true); o.dial != nil {
		t.Fatal(o.dial)
	}
	return
}

func TestRemoveRotatePropagates(t *testing.T) {
	a, b, c := group3(t)
	addr := serve(t, b, nil)
	// A learns about C through B.
	if _, err := dialSync(t, a, addr, b.id()); err != nil {
		t.Fatal(err)
	}
	if len(a.Snapshot().Members) != 3 {
		t.Fatal("A did not learn C")
	}

	// C has a live session at B.
	cancelled := make(chan struct{})
	release, err := b.Register(c.id(), PurposeControl, true, func() { close(cancelled) })
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	rm, err := a.Remove(c.id(), true)
	if err != nil {
		t.Fatal(err)
	}
	if _, pend := a.Progress(rm.ID()); len(pend) != 1 {
		t.Fatalf("B should be pending: %v", pend)
	}
	res, err := dialSync(t, a, addr, b.id())
	if err != nil {
		t.Fatal(err)
	}
	if !res.KeySent {
		t.Fatal("new K not delivered")
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("C's session survived the tombstone")
	}
	if v, ok := b.HeldKey(); !ok || v != rm.ID() {
		t.Fatal("B does not hold the new key version")
	}
	if _, pend := a.Progress(rm.ID()); len(pend) != 0 {
		t.Fatalf("receipt did not update progress: %v", pend)
	}
	// The removed device cannot sync, let alone fetch the new K.
	if _, err := dialSync(t, c, addr, b.id()); err == nil {
		t.Fatal("removed member synced")
	}
	// Dialing B but reaching A's certificate fails the TLS check.
	addrA := serve(t, a, nil)
	a.SetPolicy(Policy{AllowInbound: true})
	if _, err := dialSync(t, b, addrA, c.id()); err == nil {
		t.Fatal("wrong target accepted")
	}
	if _, err := dialSync(t, b, addrA, a.id()); err != nil {
		t.Fatal(err)
	}
}

func TestForkStopsControl(t *testing.T) {
	a, b := newDev(t, "A"), newDev(t, "B")
	pair(t, a, b, [2]bool{true, true}, true)
	a.SetPolicy(Policy{AllowInbound: true})
	if _, err := a.Rotate(); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Rotate(); err != nil {
		t.Fatal(err)
	}
	addr := serve(t, b, nil)
	if _, err := dialSync(t, a, addr, b.id()); !errors.Is(err, ErrFork) {
		t.Fatalf("want fork, got %v", err)
	}
	for _, d := range []*dev{a, b} {
		s := d.Snapshot()
		if !s.Conflict {
			t.Fatalf("%s not in conflict", d.name)
		}
		if _, err := d.Remove(ID{}, false); !errors.Is(err, ErrConflict) {
			t.Fatal("ops allowed in conflict")
		}
	}
	pub := b.g.State().Members[a.id()]
	if _, err := b.Authorize(pub, PurposeControl, true); !errors.Is(err, ErrConflict) {
		t.Fatal("control allowed in conflict")
	}
	// Conflict survives a restart.
	b2 := reopen(t, b)
	if !b2.Snapshot().Conflict {
		t.Fatal("conflict lost")
	}
	// Rebuild: forget locally, start over.
	if err := b2.Forget(); err != nil || b2.Snapshot().Group != (ID{}) {
		t.Fatal(err)
	}
}

func reopen(t *testing.T, d *dev) *dev {
	t.Helper()
	d.store.Close()
	st, err := OpenStore(d.dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	n, err := OpenNode(st, d.keys)
	if err != nil {
		t.Fatal(err)
	}
	return &dev{Node: n, dir: d.dir, keys: d.keys, name: d.name}
}

func TestControl(t *testing.T) {
	a, b := newDev(t, "A"), newDev(t, "B")
	pair(t, a, b, [2]bool{true, true}, true)
	got := make(chan string, 1)
	addr := serve(t, b, func(ctx context.Context, conn net.Conn, peer MemberInfo) {
		buf := make([]byte, 5)
		io.ReadFull(conn, buf)
		got <- peer.Name + ":" + string(buf)
	})
	dial := func() (net.Conn, error) {
		raw, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		return a.DialControl(context.Background(), raw, b.id())
	}
	// No confirmation UI: refused.
	if _, err := dial(); err == nil {
		t.Fatal("control without approval UI")
	}
	approve := false
	b.Approve = func(ctx context.Context, p MemberInfo) bool { return approve }
	if _, err := dial(); err == nil {
		t.Fatal("control while denied")
	}
	approve = true
	conn, err := dial()
	if err != nil {
		t.Fatal(err)
	}
	conn.Write([]byte("hello"))
	if s := <-got; s != "A:hello" {
		t.Fatalf("got %q", s)
	}
	conn.Close()

	// Closing inbound while a request waits for approval: the late
	// approval does not count.
	waiting := make(chan struct{})
	b.Approve = func(ctx context.Context, p MemberInfo) bool {
		close(waiting)
		<-ctx.Done()
		return true
	}
	go func() { <-waiting; b.SetPolicy(Policy{}) }()
	if _, err := dial(); err == nil {
		t.Fatal("late approval accepted")
	}
	// Unattended skips the dialog.
	b.SetPolicy(Policy{AllowInbound: true, Unattended: true})
	b.Approve = nil
	if conn, err := dial(); err != nil {
		t.Fatal(err)
	} else {
		conn.Close()
	}
}

func TestStoreCrashAndLock(t *testing.T) {
	a := newDev(t, "A")
	if _, err := OpenStore(a.dir); !errors.Is(err, ErrLocked) {
		t.Fatalf("second writer: %v", err)
	}
	if _, err := a.CreateGroup(); err != nil {
		t.Fatal(err)
	}
	// Crash after writing a new slot but before replacing members.json.
	a.keys.Put(KeySlot(a.Snapshot().Group, ID{7}), NewGroupKey())
	a2 := reopen(t, a)
	slots, _ := a2.keys.List()
	if len(slots) != 2 {
		t.Fatalf("orphan slot kept: %v", slots)
	}
	if a2.Err() != nil || !a2.Snapshot().InGroup {
		t.Fatal(a2.Err())
	}
	// A referenced key slot that is missing pauses key services.
	v, _ := a2.HeldKey()
	a2.keys.Delete(KeySlot(a2.Snapshot().Group, v))
	a3 := reopen(t, a2)
	if a3.Err() == nil || a3.ListenEndpoints() != nil {
		t.Fatal("missing K not detected")
	}
}

func TestSyncAfterPeerEnablesInbound(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		name := "advertised relay"
		if legacy {
			name = "old profile without relay"
		}
		t.Run(name, func(t *testing.T) {
			controller, desktop := newDev(t, "Controller"), newDev(t, "Desktop")
			if o := pair(t, controller, desktop, [2]bool{true, true}, false); o.dial != nil || o.serv != nil {
				t.Fatal(o.dial, o.serv)
			}
			home := Relay{Region: 304}
			remote := Relay{Region: 303}
			if legacy {
				remote = home
			}
			if err := controller.SetProfile(controller.name, "darwin", &home); err != nil {
				t.Fatal(err)
			}
			if err := desktop.SetProfile(desktop.name, "darwin", &remote); err != nil {
				t.Fatal(err)
			}
			if !legacy {
				// Share the signed off-state profile, including the remote's
				// different relay, by the existing device-code recovery path.
				if o := pair(t, controller, desktop, [2]bool{true, true}, false); o.dial != nil || o.serv != nil {
					t.Fatal(o.dial, o.serv)
				}
			}
			if len(desktop.ListenEndpoints()) != 0 || len(controller.DialEndpoints(desktop.id())) != 0 {
				t.Fatal("disabled desktop accepted a listener or offered control")
			}
			if err := desktop.SetPolicy(Policy{AllowInbound: true}); err != nil {
				t.Fatal(err)
			}
			desktop = reopen(t, desktop)
			candidates, listeners := controller.SyncEndpoints(desktop.id()), desktop.ListenEndpoints()
			if len(candidates) != 1 || len(listeners) != 1 || candidates[0].Control || candidates[0].Relay != remote {
				t.Fatal("expected a sync-only candidate on the desktop relay")
			}
			dialAddr, err := candidates[0].Addr()
			listenAddr, listenErr := listeners[0].Addr()
			if err != nil || listenErr != nil || dialAddr != listenAddr {
				t.Fatal("discovery did not reach the advertised listener")
			}
			if _, err := dialSync(t, controller, serve(t, desktop, nil), desktop.id()); err != nil {
				t.Fatal(err)
			}
			endpoints := controller.DialEndpoints(desktop.id())
			if !memberRow(controller.Snapshot(), desktop.id()).Inbound || len(endpoints) != 1 || !endpoints[0].Control {
				t.Fatal("authenticated sync did not refresh the desktop entry")
			}
			if err := desktop.SetPolicy(Policy{}); err != nil {
				t.Fatal(err)
			}
			if len(desktop.ListenEndpoints()) != 0 || memberRow(desktop.Snapshot(), desktop.id()).Inbound {
				t.Fatal("disabling inbound did not immediately revoke listening and update the profile")
			}
		})
	}
}

func TestEndpointsAndMigration(t *testing.T) {
	a, b := newDev(t, "A"), newDev(t, "B")
	pair(t, a, b, [2]bool{true, true}, true)
	old := Relay{Region: 301}
	if err := b.SetProfile("B", "darwin", &old); err != nil {
		t.Fatal(err)
	}
	nw := Relay{Address: "derp.example.com:8443"}
	if err := b.SetProfile("B", "darwin", &nw); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Rotate(); err != nil {
		t.Fatal(err)
	}
	eps := b.ListenEndpoints()
	// Current/previous key × new/old relay; only the first takes control.
	if len(eps) != 4 || !eps[0].Control || eps[1].Control || eps[0].Relay != nw {
		t.Fatalf("%d endpoints", len(eps))
	}
	addrs := map[string]bool{}
	for _, e := range eps {
		ad, err := e.Addr()
		if err != nil {
			t.Fatal(err)
		}
		addrs[string(ad)] = true
	}
	if len(addrs) != 4 {
		t.Fatal("endpoints share an address")
	}
	// After A syncs and acks, B can end both transitions.
	addr := serve(t, b, nil)
	if _, err := dialSync(t, a, addr, b.id()); err != nil {
		t.Fatal(err)
	}
	if err := b.Maintain(); err != nil {
		t.Fatal(err)
	}
	if eps := b.ListenEndpoints(); len(eps) != 1 || eps[0].Relay != nw {
		t.Fatalf("transition not ended: %d endpoints", len(eps))
	}
	if eps := a.DialEndpoints(b.id()); len(eps) == 0 || !eps[0].Control {
		t.Fatal("A cannot dial B")
	}
}

// A phone that creates the group and invites a desktop must learn the
// desktop's address from pairing alone: it never listens, so no sync can
// teach it later.
func TestPairCarriesProfiles(t *testing.T) {
	phone, desk := newDev(t, "Phone"), newDev(t, "Desk")
	tokens := &TokenBook{}
	c1, c2 := net.Pipe()
	yes := func(context.Context, PairPrompt) (bool, bool) { return true, true }
	errc := make(chan error, 1)
	go func() {
		errc <- desk.PairServe(context.Background(), c2, tokens, PairOptions{Name: "Desk", Platform: "darwin", CanInbound: true,
			Relay: &Relay{Region: 303}, Confirm: yes})
	}()
	if err := phone.PairDial(context.Background(), c1, tokens.Issue("s"), PairOptions{Name: "Phone", Platform: "android", Confirm: yes}); err != nil {
		t.Fatal(err)
	}
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	eps := phone.DialEndpoints(desk.id())
	if len(eps) != 1 || eps[0].Relay != (Relay{Region: 303}) || !eps[0].Control {
		t.Fatalf("phone endpoints for desk: %+v", eps)
	}
	if m := memberRow(phone.Snapshot(), desk.id()); m.Name != "Desk" || !m.Inbound {
		t.Fatalf("%+v", m)
	}
}

// One side declining closes the other side's prompt instead of leaving it
// open until a timeout.
func TestPairPeerCancelClosesPrompt(t *testing.T) {
	a, b := newDev(t, "A"), newDev(t, "B")
	tokens := &TokenBook{}
	c1, c2 := net.Pipe()
	closed := make(chan struct{})
	go b.PairServe(context.Background(), c2, tokens, PairOptions{Name: "B", CanInbound: true,
		Confirm: func(ctx context.Context, p PairPrompt) (bool, bool) {
			<-ctx.Done() // the person never answers here
			close(closed)
			return true, false
		}})
	err := a.PairDial(context.Background(), c1, tokens.Issue("s"), PairOptions{Name: "A",
		Confirm: func(context.Context, PairPrompt) (bool, bool) { return false, false }})
	if err == nil {
		t.Fatal("pairing succeeded")
	}
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("B's prompt stayed open after A declined")
	}
	if b.Snapshot().InGroup || a.Snapshot().InGroup {
		t.Fatal("authorization added")
	}
}

func TestSameGroupRecovery(t *testing.T) {
	a, b := newDev(t, "A"), newDev(t, "B")
	yes := [2]bool{true, true}
	if o := pair(t, a, b, yes, true); o.dial != nil || o.serv != nil {
		t.Fatal(o.dial, o.serv)
	}
	loseKey := func(d *dev) *dev {
		v, _ := d.HeldKey()
		if err := d.keys.Delete(KeySlot(d.Snapshot().Group, v)); err != nil {
			t.Fatal(err)
		}
		return reopen(t, d)
	}
	a = loseKey(a)
	o := pair(t, a, b, yes, true)
	if o.dial != nil || o.serv != nil || o.codes[0] == "" || o.codes[0] != o.codes[1] {
		t.Fatalf("recovery: %+v", o)
	}
	a = reopen(t, a)
	if _, held := a.HeldKey(); !held || a.Err() != nil {
		t.Fatal("recovered key not persisted")
	}
	a, b = loseKey(a), loseKey(b)
	if o := pair(t, b, a, yes, true); o.dial == nil || o.serv == nil {
		t.Fatal("recovery succeeded without any source of the key")
	}
}

func TestLeaveAnnouncesRemoval(t *testing.T) {
	a, b := newDev(t, "A"), newDev(t, "B")
	if o := pair(t, a, b, [2]bool{true, true}, true); o.dial != nil {
		t.Fatal(o.dial)
	}
	if err := b.SetProfile("Desk", "darwin", &Relay{Region: 301}); err != nil {
		t.Fatal(err)
	}
	addr := serve(t, b, nil)
	if _, err := dialSync(t, a, addr, b.id()); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Leave(); err != nil {
		t.Fatal(err)
	}
	eps := a.DialEndpoints(b.id())
	if len(eps) != 1 || eps[0].Control || len(a.ListenEndpoints()) != 0 {
		t.Fatal("leaving requires a sync-only outbound address")
	}
	// The peer closes the sync connection as soon as it commits our removal.
	dialSync(t, a, addr, b.id())
	if memberRow(b.Snapshot(), a.id()).ID == a.id() {
		t.Fatal("peer did not persist departure")
	}
}

func TestControlRegistration(t *testing.T) {
	for _, action := range []string{"remove while waiting", "remove while active", "rotate", "change relay"} {
		t.Run(action, func(t *testing.T) {
			a, b := newDev(t, "A"), newDev(t, "B")
			if o := pair(t, a, b, [2]bool{true, true}, true); o.dial != nil {
				t.Fatal(o.dial)
			}
			if err := b.SetProfile("B", "darwin", &Relay{Region: 301}); err != nil {
				t.Fatal(err)
			}
			prompt, answer := make(chan struct{}, 1), make(chan struct{})
			b.Approve = func(ctx context.Context, _ MemberInfo) bool {
				prompt <- struct{}{}
				select {
				case <-ctx.Done():
					return false
				case <-answer:
					return true
				}
			}
			ep := b.ListenEndpoints()[0]
			handler := b.EndpointHandler(ep, func(ctx context.Context, conn net.Conn, _ MemberInfo) { io.Copy(io.Discard, conn) })
			addr := serveHandler(t, b, handler)
			dial := func() (net.Conn, error) {
				raw, err := net.Dial("tcp", addr)
				if err != nil {
					return nil, err
				}
				return a.DialControl(context.Background(), raw, b.id())
			}
			if action == "remove while waiting" {
				done := make(chan error, 1)
				go func() { _, err := dial(); done <- err }()
				defer close(answer)
				select {
				case <-prompt:
				case <-time.After(3 * time.Second):
					t.Fatal("no approval prompt")
				}
				if _, err := a.Remove(b.id(), false); err != nil {
					t.Fatal(err)
				}
				select {
				case err := <-done:
					if err == nil {
						t.Fatal("revoked request approved")
					}
				case <-time.After(3 * time.Second):
					t.Fatal("outbound approval still waiting")
				}
				return
			}
			close(answer)
			conn, err := dial()
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			switch action {
			case "remove while active":
				_, err = a.Remove(b.id(), false)
			case "rotate":
				_, err = b.Rotate()
			case "change relay":
				err = b.SetProfile("B", "darwin", &Relay{Region: 302})
			}
			if err != nil {
				t.Fatal(err)
			}
			conn.SetReadDeadline(time.Now().Add(2 * time.Second))
			if _, err := conn.Read(make([]byte, 1)); err == nil {
				t.Fatal("revoked connection still readable")
			} else if e, ok := err.(net.Error); ok && e.Timeout() {
				t.Fatal("connection survived until read deadline")
			}
			if action != "remove while active" {
				if _, err := dialSync(t, a, addr, b.id()); err != nil {
					t.Fatal("old endpoint must still sync", err)
				}
				if c, err := dial(); err == nil {
					c.Close()
					t.Fatal("old endpoint still granted control")
				}
			}
		})
	}
}

func TestPairPromptHasOwnDeadline(t *testing.T) {
	a, b := newDev(t, "A"), newDev(t, "B")
	tokens := &TokenBook{}
	c1, c2 := net.Pipe()
	done := make(chan error, 1)
	bounded := make(chan bool, 1)
	go func() {
		done <- b.PairServe(context.Background(), c2, tokens, PairOptions{Confirm: func(ctx context.Context, _ PairPrompt) (bool, bool) {
			end, ok := ctx.Deadline()
			bounded <- ok && time.Until(end) > 0 && time.Until(end) <= PairConfirmTime
			return false, false
		}})
	}()
	a.PairDial(context.Background(), c1, tokens.Issue("s"), PairOptions{Confirm: func(context.Context, PairPrompt) (bool, bool) { return true, false }})
	if err := <-done; err == nil || !<-bounded {
		t.Fatal("receiving desktop prompt requires a bounded local deadline")
	}
}
