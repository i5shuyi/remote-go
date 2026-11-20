package backend

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/i5shuyi/remote-go/internal/client"
	"github.com/i5shuyi/remote-go/internal/host"
	"github.com/i5shuyi/remote-go/internal/members"
	"github.com/i5shuyi/remote-go/internal/tunnel"
)

func testNode(t *testing.T) *members.Node {
	t.Helper()
	st, err := members.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	n, err := members.OpenNode(st, &members.MemKeyStore{})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// pairNodes pairs a (controller) with b (desktop) over an in-memory pipe.
func pairNodes(t *testing.T, a, b *members.Node, relay ...tunnel.MemberRelay) {
	t.Helper()
	tokens := &members.TokenBook{}
	c1, c2 := net.Pipe()
	yes := func(context.Context, members.PairPrompt) (bool, bool) { return true, true }
	errc := make(chan error, 1)
	opts := members.PairOptions{Name: "Desk", Platform: "darwin", CanInbound: true, Confirm: yes}
	if len(relay) > 0 {
		opts.Relay = &relay[0]
	}
	go func() { errc <- b.PairServe(context.Background(), c2, tokens, opts) }()
	if err := a.PairDial(context.Background(), c1, tokens.Issue("s"), members.PairOptions{Name: "Phone", Platform: "android", Confirm: yes}); err != nil {
		t.Fatal(err)
	}
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
}

// The whole member path except the tailcat transport: mutual TLS, sync,
// local approval, the host's desktop protocol and the client.
func TestMemberSessionEndToEnd(t *testing.T) {
	phone, desk := testNode(t), testNode(t)
	pairNodes(t, phone, desk)
	if !desk.Policy().AllowInbound {
		t.Fatal("desktop should allow inbound after ticking the box")
	}

	h, err := host.New(host.Config{Name: "Desk", Code: "123456", ViewOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	asked := 0
	desk.Approve = func(ctx context.Context, p members.MemberInfo) bool {
		mu.Lock()
		asked++
		mu.Unlock()
		return p.Name == "Phone"
	}
	cfg, err := desk.ServerTLS()
	if err != nil {
		t.Fatal(err)
	}
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	srv := &http.Server{Handler: desk.Handler(func(ctx context.Context, conn net.Conn, p members.MemberInfo) {
		h.ServeMember(ctx, conn, host.MemberPeer{ID: p.ID.String(), Name: p.Name, ViewOnly: true})
	})}
	go srv.Serve(tls.NewListener(ln, cfg))
	defer srv.Close()

	deskID, _ := desk.Self()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	raw, _ := net.Dial("tcp", ln.Addr().String())
	conn, err := phone.DialControl(ctx, raw, deskID.ID())
	if err != nil {
		t.Fatal(err)
	}
	infos := make(chan client.Info, 1)
	r, err := sessionOver(ctx, conn, nil, client.Config{Quality: 50, FPS: 5, Width: 640},
		client.Handler{OnInfo: func(i client.Info) { infos <- i }})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	select {
	case i := <-infos:
		if i.Name != "Desk" || i.Control {
			t.Fatalf("info %+v: view-only member session must not offer control", i)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no greeting")
	}
	if asked != 1 {
		t.Fatalf("approval asked %d times", asked)
	}
	ss := h.Sessions()
	if len(ss) != 1 || ss[0].Member == "" || !ss[0].ViewOnly {
		t.Fatalf("sessions %+v", ss)
	}
	// Refreshing the device code kicks only code sessions.
	h.KickCodeSessions()
	time.Sleep(200 * time.Millisecond)
	if len(h.Sessions()) != 1 {
		t.Fatal("member session ended with the device code")
	}
	// Removing the phone ends its live session.
	if _, err := desk.Remove(MemberIDOf(phone), false); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(h.Sessions()) != 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if len(h.Sessions()) != 0 {
		t.Fatal("removed member's session survived")
	}
}

func MemberIDOf(n *members.Node) members.ID { id, _ := n.Self(); return id.ID() }

func TestPromptsAndBarrier(t *testing.T) {
	a := &App{}
	a.ctx = context.Background()
	h, _ := host.New(host.Config{Name: "x", Code: "1", ViewOnly: true})
	a.host = h
	a.UIAttached = func() bool { return false }
	if ok, _ := a.ask(context.Background(), PromptDTO{Kind: "control"}); ok {
		t.Fatal("prompt without a window must be refused")
	}
	a.UIAttached = nil
	done := make(chan [2]bool, 1)
	go func() {
		ok, in := a.ask(context.Background(), PromptDTO{Kind: "pair", CanInbound: true})
		done <- [2]bool{ok, in}
	}()
	var id int
	for i := 0; i < 100 && id == 0; i++ {
		time.Sleep(10 * time.Millisecond)
		if ps := a.membersDTO().Prompts; len(ps) == 1 {
			id = ps[0].ID
		}
	}
	if id == 0 {
		t.Fatal("prompt not published")
	}
	if !hostPaused(h) {
		t.Fatal("input barrier not raised before the prompt")
	}
	if err := a.AnswerPrompt(id, true, true); err != nil {
		t.Fatal(err)
	}
	if r := <-done; !r[0] || !r[1] {
		t.Fatal(r)
	}
	if hostPaused(h) || len(a.membersDTO().Prompts) != 0 {
		t.Fatal("barrier or prompt left behind")
	}
	if a.AnswerPrompt(id, true, false) == nil {
		t.Fatal("answered a stale prompt")
	}
}

func hostPaused(h *host.Server) bool { return h.InputPaused() }

// A phone that paired with a desktop reaches it over tailcat with no codes.
// RG_TAILCAT_E2E=1 go test ./internal/backend -run TestMembersTailcat -v
func TestMembersTailcat(t *testing.T) {
	if os.Getenv("RG_TAILCAT_E2E") == "" {
		t.Skip("set RG_TAILCAT_E2E=1 (needs the internet)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	region, err := tunnel.NearestRegion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	phone, desk := testNode(t), testNode(t)
	pairNodes(t, phone, desk, tunnel.MemberRelay{Region: int(region)})
	eps := desk.ListenEndpoints()
	if len(eps) != 1 || !eps[0].Control {
		t.Fatalf("desktop endpoints: %d", len(eps))
	}
	cfg, _ := desk.ServerTLS()
	ln, err := tunnel.ListenMember(ctx, eps[0].MemberEndpoint, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	srv := &http.Server{Handler: desk.Handler(nil)}
	go srv.Serve(tls.NewListener(ln, cfg))
	defer srv.Close()
	dial := phone.DialEndpoints(MemberIDOf(desk))
	if len(dial) == 0 {
		t.Fatal("pairing did not carry the desktop's address")
	}
	a1, _ := dial[0].Addr()
	a2, _ := eps[0].Addr()
	if a1 != a2 {
		t.Fatal("phone and desktop derive different member addresses")
	}
	tc, err := tunnel.DialMember(ctx, dial[0].MemberEndpoint, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tc.Close()
	raw, err := tc.DialContext(ctx, "tcp", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := phone.DialSync(ctx, raw, MemberIDOf(desk)); err != nil {
		t.Fatal(err)
	}
}
