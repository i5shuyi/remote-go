package members

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"
)

// PairConfirmTime bounds how long both people have to compare the code.
const PairConfirmTime = 2 * time.Minute

// PairTokenTTL bounds how long a pairing token can be redeemed.
const PairTokenTTL = 2 * time.Minute

// TokenBook issues the single-use pairing tokens a desktop hands to an
// authenticated device-code session.
type TokenBook struct {
	mu sync.Mutex
	m  map[string]tokenEntry
}

type tokenEntry struct {
	session string
	expires time.Time
}

// Issue creates a 32-byte token bound to session.
func (b *TokenBook) Issue(session string) B64 {
	t := make([]byte, 32)
	rand.Read(t)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.m == nil {
		b.m = map[string]tokenEntry{}
	}
	b.m[string(t)] = tokenEntry{session, time.Now().Add(PairTokenTTL)}
	return t
}

// Redeem consumes a token; each binds at most one connection.
func (b *TokenBook) Redeem(t []byte) (session string, ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	e, ok := b.m[string(t)]
	delete(b.m, string(t))
	if !ok || time.Now().After(e.expires) {
		return "", false
	}
	return e.session, true
}

// Revoke drops a session's tokens when its device-code connection closes.
func (b *TokenBook) Revoke(session string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for k, e := range b.m {
		if e.session == session {
			delete(b.m, k)
		}
	}
}

// PairPrompt is what both screens show for local confirmation.
type PairPrompt struct {
	Code     string // compare the whole value on both devices
	Role     string // "inviter" | "joiner" | "recover"
	Group    ID     // zero when the group is created on confirmation
	PeerKey  ed25519.PublicKey
	PeerID   ID
	PeerName string
	NewGroup bool
}

// PairOptions configure one pairing.
type PairOptions struct {
	// Confirm must raise the local input barrier (pause remote input,
	// release held keys, drop queued events) before showing p, and return
	// true only on a local, non-default "allow". A joining desktop also
	// returns whether the person ticked "允许我的设备直连". Nil refuses.
	Confirm func(ctx context.Context, p PairPrompt) (ok, allowInbound bool)
	// Name and Platform describe this device to the other one.
	Name, Platform string
	// CanInbound is false for controller-only devices (Android), which
	// never accept connections whatever the confirmation says.
	CanInbound bool
	// Relay is where this desktop's member listener is homed. Pairing
	// carries both profiles, so a phone that never listens still learns
	// the address of the desktop it paired with.
	Relay *Relay
}

type pairHello struct {
	Token B64    `json:"token,omitempty"`
	Group ID     `json:"group,omitzero"`
	Name  string `json:"name"`
}

type pairTxn struct {
	Txn ID `json:"txn,omitzero"`
}

type pairConfirm struct {
	OK  bool `json:"ok"`
	Txn ID   `json:"txn,omitzero"`
}

type pairGroup struct {
	Group ID `json:"group"`
}

type pairIntent struct {
	Intent Signed `json:"intent"`
}

type pairDeliver struct {
	Error    string   `json:"error,omitempty"`
	Genesis  *Signed  `json:"genesis,omitempty"`
	History  []Signed `json:"history,omitempty"`
	Profiles []Signed `json:"profiles,omitempty"`
	Key      B64      `json:"key,omitempty"`
}

type pairAck struct {
	Ack     *Signed `json:"ack,omitempty"`
	Profile *Signed `json:"profile,omitempty"`
}

// pairIdentity is the member identity, or the persisted pending identity
// of a device that is not in a group.
func (n *Node) pairIdentity() (Identity, ID, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.broken != nil {
		return Identity{}, ID{}, n.broken
	}
	if n.g != nil {
		if !n.g.State().Active(n.self.ID()) {
			return Identity{}, ID{}, ErrNotMember
		}
		return n.self, n.g.ID(), nil
	}
	if p := n.f.Pending; p != nil {
		if priv, err := n.keys.Get(pendingSlot(p.Member)); err == nil && len(priv) == ed25519.PrivateKeySize {
			return Identity{Priv: ed25519.PrivateKey(priv)}, ID{}, nil
		}
	}
	id := NewIdentity()
	n.f.Pending = &PendingJoin{Member: id.ID()}
	if err := n.commitLocked(map[string][]byte{pendingSlot(id.ID()): id.Priv}); err != nil {
		return Identity{}, ID{}, err
	}
	return id, ID{}, nil
}

// PairDial pairs over conn, dialed to the desktop's PairPort with a token
// from its authenticated device-code session.
func (n *Node) PairDial(ctx context.Context, conn net.Conn, token []byte, opts PairOptions) error {
	return n.pair(ctx, conn, true, token, nil, opts)
}

// PairServe pairs over a connection accepted on PairPort.
func (n *Node) PairServe(ctx context.Context, conn net.Conn, tokens *TokenBook, opts PairOptions) error {
	return n.pair(ctx, conn, false, nil, tokens, opts)
}

func (n *Node) pair(ctx context.Context, raw net.Conn, dialer bool, token []byte, tokens *TokenBook, opts PairOptions) error {
	defer raw.Close()
	me, myGroup, err := n.pairIdentity()
	if err != nil {
		return err
	}
	var peerPub ed25519.PublicKey
	cfg, err := baseTLS(me, func(pub ed25519.PublicKey) error { peerPub = pub; return nil })
	if err != nil {
		return err
	}
	deadline(raw, handshakeTime)
	var tc *tls.Conn
	if dialer {
		tc = tls.Client(raw, cfg)
	} else {
		tc = tls.Server(raw, cfg)
	}
	if err := tc.HandshakeContext(ctx); err != nil {
		return err
	}
	// Closing raw (deferred above) ends the stream; tls.Conn.Close would
	// block on close_notify while the peer closes too.
	stop := context.AfterFunc(ctx, func() { raw.Close() })
	defer stop()
	fr := newFrames(tc, tc)

	hello := pairHello{Group: myGroup, Name: opts.Name}
	if dialer {
		hello.Token = token
	}
	var peer pairHello
	if err := fr.exchange(hello, &peer); err != nil {
		return err
	}
	if !dialer {
		if tokens == nil {
			return errors.New("配对令牌无效")
		}
		if _, ok := tokens.Redeem(peer.Token); !ok {
			return errors.New("配对令牌无效或已使用，请从设备码会话重新发起")
		}
		token = peer.Token
	}

	var role string
	switch {
	case !myGroup.IsZero() && !peer.Group.IsZero() && myGroup == peer.Group:
		role = "recover"
	case !myGroup.IsZero() && !peer.Group.IsZero():
		return errors.New("两台设备属于不同的组：请先在要迁移的设备上退出原组")
	case !myGroup.IsZero() || peer.Group.IsZero() && dialer:
		role = "inviter"
	default:
		role = "joiner"
	}
	inviterPub, joinerPub := me.Public(), peerPub
	if role == "joiner" {
		inviterPub, joinerPub = peerPub, me.Public()
	}
	dir := "joiner-dials"
	if (role == "inviter") == dialer {
		dir = "inviter-dials"
	}
	group := myGroup
	if group.IsZero() {
		group = peer.Group
	}
	code, err := PairCode(tc.ConnectionState(), PairContext{Token: token, Group: group, Inviter: B64(inviterPub), Joiner: B64(joinerPub), Direction: dir})
	if err != nil {
		return err
	}

	// Transaction: a joiner retrying a committed pairing names its txn.
	var myTxn pairTxn
	if role == "joiner" {
		n.mu.Lock()
		if p := n.f.Pending; p != nil && p.Group == group && !group.IsZero() {
			myTxn.Txn = p.Txn
		}
		n.mu.Unlock()
	}
	var peerTxn pairTxn
	if err := fr.exchange(myTxn, &peerTxn); err != nil {
		return err
	}

	// The person compares codes on both screens; give them time, and end
	// the local prompt as soon as the other side cancels, drops or times out.
	deadline(raw, PairConfirmTime)
	cctx, ccancel := context.WithCancel(ctx)
	defer ccancel()
	peerc := make(chan pairConfirm, 1)
	perr := make(chan error, 1)
	go func() {
		var pc pairConfirm
		if err := fr.recv(&pc); err != nil {
			perr <- err
			ccancel()
			return
		}
		if !pc.OK {
			ccancel()
		}
		peerc <- pc
	}()
	ok, inbound := false, false
	if opts.Confirm != nil {
		ok, inbound = opts.Confirm(cctx, PairPrompt{Code: code, Role: role, Group: group, PeerKey: peerPub, PeerID: MemberID(peerPub),
			PeerName: peer.Name, NewGroup: group.IsZero()})
	}
	ok = ok && cctx.Err() == nil
	conf := pairConfirm{OK: ok}
	if role == "inviter" {
		conf.Txn = n.chooseTxn(peerTxn.Txn, MemberID(peerPub))
	}
	serr := fr.send(conf)
	var peerConf pairConfirm
	select {
	case peerConf = <-peerc:
	case err := <-perr:
		return fmt.Errorf("配对已中断：%w", err)
	}
	if serr != nil {
		return serr
	}
	if !ok || !peerConf.OK {
		return errors.New("配对已取消")
	}
	deadline(raw, handshakeTime)

	switch role {
	case "recover":
		_, err := n.sync(fr, MemberID(peerPub))
		return err
	case "inviter":
		return n.invite(fr, me, MemberID(peerPub), conf.Txn, opts)
	default:
		return n.join(fr, me, MemberID(peerPub), peerConf.Txn, opts, inbound && opts.CanInbound)
	}
}

func (n *Node) chooseTxn(proposed, joiner ID) ID {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, p := range n.f.Pairings {
		if p.Txn == proposed && p.Member == joiner {
			return proposed
		}
	}
	var t ID
	rand.Read(t[:])
	return t
}

func (n *Node) invite(fr *frames, me Identity, joiner, txn ID, opts PairOptions) error {
	n.mu.Lock()
	if n.g == nil { // first pairing of two ungrouped devices: create now
		k := NewGroupKey()
		g, err := Create(me, k)
		if err != nil {
			n.mu.Unlock()
			return err
		}
		n.g, n.self, n.k = g, me, map[ID][]byte{g.ID(): k}
		n.f.Self, n.f.KeyCurrent, n.f.Pending = me.ID(), g.ID(), nil
		slots := map[string][]byte{identitySlot: me.Priv, KeySlot(g.ID(), g.ID()): k}
		if err := n.commitLocked(slots); err != nil {
			n.mu.Unlock()
			return err
		}
	}
	group := n.g.ID()
	n.mu.Unlock()
	n.SetProfile(opts.Name, opts.Platform, n.inboundProfile(), opts.Relay)

	var in pairIntent
	if err := fr.exchange(pairGroup{Group: group}, &in); err != nil {
		return err
	}
	var intent Intent
	if err := in.Intent.open(TypeIntent, &intent); err != nil {
		return err
	}
	if intent.Group != group || intent.Inviter != me.ID() || intent.Txn != txn || MemberID(ed25519.PublicKey(intent.Key)) != joiner {
		return errors.New("入组意愿与本次配对不符")
	}

	d := n.addForPairing(me, in.Intent, txn, joiner)
	if err := fr.send(d); err != nil {
		return err
	}
	if d.Error != "" {
		return errors.New(d.Error)
	}
	var ack pairAck
	if err := fr.recv(&ack); err != nil {
		return fmt.Errorf("已添加，待对方确认：%w", err)
	}
	if ack.Profile != nil {
		n.mu.Lock()
		if _, err := n.g.ApplyProfile(*ack.Profile); err == nil {
			n.commitLocked(nil)
		}
		n.mu.Unlock()
	}
	if _, ok := n.applyAck(joiner, ack.Ack); !ok {
		return errors.New("已添加，待对方确认")
	}
	return nil
}

// addForPairing commits the add once per transaction and returns the
// current history and current K.
func (n *Node) addForPairing(me Identity, intent Signed, txn, joiner ID) pairDeliver {
	n.mu.Lock()
	defer n.mu.Unlock()
	if err := n.usableLocked(); err != nil {
		return pairDeliver{Error: err.Error()}
	}
	if n.g.Conflict() != nil {
		return pairDeliver{Error: ErrConflict.Error()}
	}
	done := false
	for _, p := range n.f.Pairings {
		if p.Txn == txn && p.Member == joiner {
			done = true
		}
	}
	if !done {
		r, err := n.g.Add(me, intent)
		if err != nil {
			return pairDeliver{Error: err.Error()}
		}
		n.f.Pairings = append(n.f.Pairings, PairResult{Txn: txn, Member: joiner, Record: r.ID()})
		if err := n.commitLocked(nil); err != nil {
			return pairDeliver{Error: err.Error()}
		}
	}
	if !n.g.State().Active(joiner) {
		return pairDeliver{Error: "该设备已被移除，须以新身份重新配对"}
	}
	k, ok := n.k[n.g.State().KeyVersion]
	if !ok || n.keyErr != nil {
		return pairDeliver{Error: "本机尚未持有当前组密钥"}
	}
	gen := n.g.Genesis()
	profiles, _, _ := n.g.Extras()
	return pairDeliver{Genesis: &gen, History: n.g.History(), Profiles: profiles, Key: k}
}

func (n *Node) inboundProfile() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.f.Policy.AllowInbound
}

func (n *Node) join(fr *frames, me Identity, inviter, txn ID, opts PairOptions, inbound bool) error {
	var pg pairGroup
	if err := fr.recv(&pg); err != nil {
		return err
	}
	n.mu.Lock()
	n.f.Pending = &PendingJoin{Member: me.ID(), Txn: txn, Group: pg.Group}
	err := n.commitLocked(nil)
	n.mu.Unlock()
	if err != nil {
		return err
	}
	intent, err := NewIntent(me, pg.Group, inviter, txn)
	if err != nil {
		return err
	}
	if err := fr.send(pairIntent{Intent: intent}); err != nil {
		return err
	}
	var d pairDeliver
	if err := fr.recv(&d); err != nil {
		return err
	}
	if d.Error != "" {
		return errors.New(d.Error)
	}
	if d.Genesis == nil || d.Genesis.ID() != pg.Group {
		return errors.New("组根与配对时不符")
	}
	g, err := Load(*d.Genesis, d.History)
	if err != nil {
		return err
	}
	rec, _, ok := g.AddRecord(me.ID())
	var c Change
	if !ok || rec.open(TypeChange, &c) != nil || c.Issuer != inviter {
		return errors.New("历史中没有本机的添加记录")
	}
	var in Intent
	if c.Add.Intent.open(TypeIntent, &in) != nil || in.Txn != txn {
		return errors.New("添加记录不属于本次配对")
	}
	s := g.State()
	if !s.Active(me.ID()) || KeyFingerprint(d.Key) != s.KeyFP {
		return errors.New("组密钥指纹不符或本机已被移除")
	}
	for _, p := range d.Profiles {
		g.ApplyProfile(p)
	}

	n.mu.Lock()
	if n.g != nil {
		n.mu.Unlock()
		return errors.New("本机已加入设备组")
	}
	n.g, n.self, n.k = g, me, map[ID][]byte{s.KeyVersion: d.Key}
	n.f.Self, n.f.KeyCurrent, n.f.KeyPrev, n.f.Pending = me.ID(), s.KeyVersion, ID{}, nil
	n.f.Policy = Policy{AllowInbound: inbound}
	err = n.commitLocked(map[string][]byte{identitySlot: me.Priv, KeySlot(g.ID(), s.KeyVersion): d.Key})
	n.mu.Unlock()
	if err != nil {
		return err
	}
	n.SetProfile(opts.Name, opts.Platform, inbound, opts.Relay)
	out := pairAck{Ack: n.newAck()}
	n.mu.Lock()
	if p, ok := n.g.ProfileRecord(me.ID()); ok {
		out.Profile = &p
	}
	n.mu.Unlock()
	return fr.send(out)
}
