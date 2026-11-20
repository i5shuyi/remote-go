package members

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/i5shuyi/remote-go/internal/tunnel"
)

// Transition is how long the previous key generation and the previous relay
// keep serving sync and catch-up when not every member has signed off.
const Transition = 90 * 24 * time.Hour

// Connection purposes.
const (
	PurposeSync    = "sync"
	PurposeControl = "control"
)

// Node is this device's member state: the single writer of members.json.
// All methods are safe for concurrent use.
type Node struct {
	mu    sync.Mutex
	store *Store
	keys  KeyStore
	now   func() time.Time

	f         *File
	g         *Group // nil when not in a group
	self      Identity
	k         map[ID][]byte // held group keys by version
	broken    error         // state could not be verified: inbound closed
	keyErr    error         // a referenced key slot is missing or wrong
	gen       uint64        // bumped by every authorization change
	sess      map[*session]struct{}
	departing bool // allow sync-only dialing until a local Leave is announced

	// Approve asks the person at this desktop to allow a control request.
	// Nil means no local confirmation UI: control needing approval fails.
	Approve ApproveFunc
	// Changed is called (without the lock) after every committed change.
	Changed func()
}

type session struct {
	member   ID
	purpose  string
	inbound  bool
	cancel   func()
	endpoint *Endpoint // receiving address of a member control request
}

// OpenNode loads the state owned by store. A state that fails validation
// leaves the node open with inbound closed; Err reports why.
func OpenNode(store *Store, keys KeyStore) (*Node, error) {
	n := &Node{store: store, keys: keys, now: time.Now, sess: map[*session]struct{}{}}
	if err := n.reloadLocked(); err != nil {
		return nil, err
	}
	n.cleanupLocked()
	return n, nil
}

func (n *Node) reloadLocked() error {
	f, err := n.store.Load()
	if err != nil {
		n.f, n.g, n.broken = &File{V: Version}, nil, fmt.Errorf("成员状态无法读取：%w", err)
		return nil
	}
	n.f, n.g, n.broken, n.keyErr, n.k = f, nil, nil, nil, map[ID][]byte{}
	if f.Genesis == nil {
		return nil
	}
	g, err := Load(*f.Genesis, f.History)
	if err == nil {
		err = g.restoreConflict(f.Conflict)
	}
	if err != nil {
		n.broken = fmt.Errorf("成员历史无法验证：%w", err)
		return nil
	}
	for _, r := range f.Profiles {
		g.ApplyProfile(r)
	}
	for _, r := range f.Acks {
		g.ApplyAck(r)
	}
	for _, r := range f.Receipts {
		g.ApplyReceipt(r)
	}
	n.g = g
	priv, err := n.keys.Get(identitySlot)
	if err != nil || len(priv) != ed25519.PrivateKeySize || MemberID(ed25519.PrivateKey(priv).Public().(ed25519.PublicKey)) != f.Self {
		n.broken = errors.New("本机成员私钥缺失或不匹配")
		return nil
	}
	n.self = Identity{Priv: ed25519.PrivateKey(priv)}
	for _, v := range []ID{f.KeyCurrent, f.KeyPrev} {
		if v.IsZero() {
			continue
		}
		k, err := n.keys.Get(KeySlot(g.ID(), v))
		fp, ok := g.KeyVersionFP(v)
		if err != nil || !ok || KeyFingerprint(k) != fp {
			if v == f.KeyCurrent {
				n.keyErr = errors.New("组密钥缺失或指纹不符，需要恢复")
			} else {
				f.KeyPrev, f.PrevUntil = ID{}, time.Time{}
			}
			continue
		}
		n.k[v] = k
	}
	return nil
}

// Err reports a state that disables member services.
func (n *Node) Err() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.broken != nil {
		return n.broken
	}
	return n.keyErr
}

// snapshotLocked mirrors the group into the persisted file.
func (n *Node) snapshotLocked() *File {
	f := *n.f
	if n.g != nil {
		gen := n.g.Genesis()
		f.Genesis = &gen
		f.History = n.g.History()
		f.Profiles, f.Acks, f.Receipts = n.g.Extras()
		f.Conflict = n.g.Conflict()
	} else {
		f.Genesis, f.History, f.Profiles, f.Acks, f.Receipts, f.Conflict = nil, nil, nil, nil, nil, nil
	}
	return &f
}

// commitLocked persists the in-memory state: new key slots first (read
// back), then members.json atomically, then revocations take effect and
// unreferenced slots are removed. On failure memory reverts to disk.
func (n *Node) commitLocked(slots map[string][]byte) error {
	for s, v := range slots {
		if err := putVerified(n.keys, s, v); err != nil {
			n.reloadLocked()
			return err
		}
	}
	f := n.snapshotLocked()
	if err := n.store.Save(f); err != nil {
		n.reloadLocked()
		n.cleanupLocked()
		return fmt.Errorf("保存成员状态失败：%w", err)
	}
	n.f = f
	n.gen++
	n.enforceLocked()
	n.cleanupLocked()
	if n.Changed != nil {
		go n.Changed()
	}
	return nil
}

// cleanupLocked deletes slots the state does not reference.
func (n *Node) cleanupLocked() {
	if n.broken != nil {
		return // never delete secrets we could not account for
	}
	keep := map[string]bool{}
	if !n.f.Self.IsZero() {
		keep[identitySlot] = true
	}
	if n.f.Pending != nil {
		keep[pendingSlot(n.f.Pending.Member)] = true
	}
	if n.g != nil {
		for _, v := range []ID{n.f.KeyCurrent, n.f.KeyPrev} {
			if !v.IsZero() {
				keep[KeySlot(n.g.ID(), v)] = true
			}
		}
	}
	slots, _ := n.keys.List()
	for _, s := range slots {
		if !keep[s] && (s == identitySlot || strings.HasPrefix(s, "k/") || strings.HasPrefix(s, "pending/")) {
			n.keys.Delete(s)
		}
	}
}

// enforceLocked cancels every session the current state no longer allows.
func (n *Node) enforceLocked() {
	for s := range n.sess {
		if n.checkSessionLocked(s) != nil {
			s.cancel()
			delete(n.sess, s)
		}
	}
}

func (n *Node) usableLocked() error {
	switch {
	case n.broken != nil:
		return n.broken
	case n.g == nil:
		return errors.New("本机未加入设备组")
	case !n.g.State().Active(n.self.ID()):
		return ErrNotMember
	}
	return nil
}

func (n *Node) authorizedLocked(member ID, purpose string, inbound bool) error {
	if err := n.usableLocked(); err != nil {
		return err
	}
	if inbound && !n.f.Policy.AllowInbound {
		return errors.New("本机未允许我的设备直连")
	}
	if n.g.Conflict() != nil {
		if purpose == PurposeSync && slices.Contains(n.g.ConflictPeers(), member) {
			return nil
		}
		return ErrConflict
	}
	if member == n.self.ID() || !n.g.State().Active(member) {
		return errors.New("身份不匹配：对方不是本组有效成员")
	}
	if purpose == PurposeControl && n.keyErr != nil {
		return n.keyErr
	}
	return nil
}

// Authorize maps a TLS peer key to a member allowed for purpose.
func (n *Node) Authorize(pub ed25519.PublicKey, purpose string, inbound bool) (ID, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	id := MemberID(pub)
	if n.g != nil {
		if k, ok := n.g.State().Members[id]; ok && !k.Equal(pub) {
			return ID{}, errors.New("公钥不符")
		}
	}
	return id, n.authorizedLocked(id, purpose, inbound)
}

// Register records a connection. The authorization check and registration
// happen under one lock, so a removal committed before cannot be missed and
// one committed after cancels it.
func (n *Node) Register(member ID, purpose string, inbound bool, cancel func()) (release func(), err error) {
	return n.register(&session{member: member, purpose: purpose, inbound: inbound, cancel: cancel})
}

func (n *Node) checkSessionLocked(s *session) error {
	if err := n.authorizedLocked(s.member, s.purpose, s.inbound); err != nil {
		return err
	}
	if ep := s.endpoint; ep != nil {
		p, ok := n.g.Profile(n.self.ID())
		v := n.g.State().KeyVersion
		if !ok || !p.Inbound || len(p.Relays) == 0 || p.Relays[0] != ep.Relay || n.f.KeyCurrent != v || !bytes.Equal(ep.KeyVersion, v[:]) {
			return errors.New("此地址仅供同步，请使用当前地址连接")
		}
	}
	return nil
}

func (n *Node) register(s *session) (func(), error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if err := n.checkSessionLocked(s); err != nil {
		return nil, err
	}
	n.sess[s] = struct{}{}
	return func() {
		n.mu.Lock()
		delete(n.sess, s)
		n.mu.Unlock()
	}, nil
}

// DisconnectAll cancels every member connection ("断开全部").
func (n *Node) DisconnectAll() {
	n.mu.Lock()
	defer n.mu.Unlock()
	for s := range n.sess {
		s.cancel()
		delete(n.sess, s)
	}
}

// Self returns this device's member identity.
func (n *Node) Self() (Identity, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.self, n.g != nil && n.broken == nil
}

// SetPolicy changes the member access policy.
func (n *Node) SetPolicy(p Policy) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.broken != nil {
		return n.broken
	}
	old := n.f.Policy
	// Keep the signed record and the switch in the same disk transaction.
	if n.g != nil && n.g.State().Active(n.self.ID()) {
		if profile, ok := n.g.Profile(n.self.ID()); ok && profile.Inbound != p.AllowInbound {
			if _, err := n.g.NewProfile(n.self, profile.Name, profile.Platform, p.AllowInbound, profile.Relays); err != nil {
				return err
			}
		}
	}
	n.f.Policy = p
	if err := n.commitLocked(nil); err != nil {
		n.f.Policy = old
		return err
	}
	return nil
}

// Policy returns the member access policy; a broken state allows nothing.
func (n *Node) Policy() Policy {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.broken != nil {
		return Policy{}
	}
	return n.f.Policy
}

// CreateGroup makes this device the first member of a new group.
func (n *Node) CreateGroup() (ID, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.broken != nil {
		return ID{}, n.broken
	}
	if n.g != nil {
		return ID{}, errors.New("本机已加入设备组")
	}
	self, k := NewIdentity(), NewGroupKey()
	g, err := Create(self, k)
	if err != nil {
		return ID{}, err
	}
	n.g, n.self, n.k = g, self, map[ID][]byte{g.ID(): k}
	n.f.Self, n.f.KeyCurrent, n.f.KeyPrev, n.f.PrevUntil = self.ID(), g.ID(), ID{}, time.Time{}
	if err := n.commitLocked(map[string][]byte{identitySlot: self.Priv, KeySlot(g.ID(), g.ID()): k}); err != nil {
		return ID{}, err
	}
	return g.ID(), nil
}

// Remove removes target, optionally rotating K in the same change.
func (n *Node) Remove(target ID, rotate bool) (Signed, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if err := n.usableLocked(); err != nil {
		return Signed{}, err
	}
	var k []byte
	if rotate && target != n.self.ID() {
		k = NewGroupKey()
	}
	r, _, err := n.g.Remove(n.self, target, k)
	if err != nil {
		return Signed{}, err
	}
	var slots map[string][]byte
	if k != nil {
		slots = n.switchKeyLocked(r.ID(), k)
	}
	return r, n.commitLocked(slots)
}

// Rotate replaces the group key without removing anyone.
func (n *Node) Rotate() (Signed, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if err := n.usableLocked(); err != nil {
		return Signed{}, err
	}
	k := NewGroupKey()
	r, err := n.g.Rotate(n.self, k)
	if err != nil {
		return Signed{}, err
	}
	return r, n.commitLocked(n.switchKeyLocked(r.ID(), k))
}

// switchKeyLocked makes version current; the old current becomes the
// previous generation with a fixed deadline, and any older one is dropped.
func (n *Node) switchKeyLocked(version ID, k []byte) map[string][]byte {
	if !n.f.KeyCurrent.IsZero() && n.f.KeyCurrent != version {
		delete(n.k, n.f.KeyPrev)
		n.f.KeyPrev, n.f.PrevUntil = n.f.KeyCurrent, n.now().Add(Transition)
	}
	n.f.KeyCurrent = version
	n.k[version] = k
	n.keyErr = nil
	return map[string][]byte{KeySlot(n.g.ID(), version): k}
}

// Leave signs this device's own removal. Pass the record to reachable
// members (Sync still works outbound), then call Forget.
func (n *Node) Leave() (Signed, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.f.Policy.AllowInbound = false
	if err := n.usableLocked(); err != nil {
		return Signed{}, err
	}
	r, _, err := n.g.Remove(n.self, n.self.ID(), nil)
	if err != nil {
		return Signed{}, err
	}
	if err := n.commitLocked(nil); err != nil {
		return Signed{}, err
	}
	n.departing = true
	return r, nil
}

// Forget stops using the group locally and deletes its identity and keys:
// after leaving, after being removed, or to rebuild after a conflict.
func (n *Node) Forget() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.g != nil {
		n.f.Left = append(n.f.Left, n.g.ID())
	}
	n.g, n.self, n.k, n.keyErr, n.broken = nil, Identity{}, map[ID][]byte{}, nil, nil
	n.f.Self, n.f.KeyCurrent, n.f.KeyPrev, n.f.PrevUntil, n.f.MigrateUntil = ID{}, ID{}, ID{}, time.Time{}, time.Time{}
	n.departing = false
	n.f.Pairings, n.f.Policy = nil, Policy{}
	return n.commitLocked(nil)
}

// HeldKey is the current key generation this device holds.
func (n *Node) HeldKey() (ID, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	_, ok := n.k[n.f.KeyCurrent]
	return n.f.KeyCurrent, ok
}

// installKeyLocked accepts K for the group's current version after checking
// its committed fingerprint.
func (n *Node) installKeyLocked(version ID, k []byte) error {
	if err := n.usableLocked(); err != nil {
		return err
	}
	s := n.g.State()
	if version != s.KeyVersion || KeyFingerprint(k) != s.KeyFP {
		return errors.New("收到的组密钥与历史承诺的指纹不符")
	}
	if _, ok := n.k[version]; ok {
		return nil
	}
	return n.commitLocked(n.switchKeyLocked(version, k))
}

// keyForLocked decides whether to deliver the current K to peer, whose ack
// shows the same head.
func (n *Node) keyForLocked(peer ID, a Ack) (ID, []byte, bool) {
	if n.usableLocked() != nil || n.g.Conflict() != nil || n.keyErr != nil {
		return ID{}, nil, false
	}
	s := n.g.State()
	k, held := n.k[s.KeyVersion]
	if !held || !s.Active(peer) || a.Member != peer || a.Head != s.Head || a.KeyVersion == s.KeyVersion {
		return ID{}, nil, false
	}
	return s.KeyVersion, k, true
}

// Maintain ends transitions that are complete or have expired.
func (n *Node) Maintain() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.usableLocked() != nil {
		return nil
	}
	changed := false
	now := n.now()
	if !n.f.KeyPrev.IsZero() && (n.g.KeySignedOff(n.self.ID(), n.f.KeyCurrent) || now.After(n.f.PrevUntil)) {
		delete(n.k, n.f.KeyPrev)
		n.f.KeyPrev, n.f.PrevUntil = ID{}, time.Time{}
		changed = true
	}
	if p, ok := n.g.Profile(n.self.ID()); ok && len(p.Relays) > 1 &&
		(n.g.MigrationDone(n.self.ID(), p.Relays[0]) || now.After(n.f.MigrateUntil)) {
		if _, err := n.g.NewProfile(n.self, p.Name, p.Platform, p.Inbound, p.Relays[:1]); err != nil {
			return err
		}
		n.f.MigrateUntil = time.Time{}
		changed = true
	}
	if !changed {
		return nil
	}
	return n.commitLocked(nil)
}

// SetProfile publishes this device's self-description. A relay different
// from the current first candidate starts a migration: the old one stays
// as second candidate until every member has a receipt, or Transition.
func (n *Node) SetProfile(name, platform string, relay *Relay) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if err := n.usableLocked(); err != nil {
		return err
	}
	inbound := n.f.Policy.AllowInbound
	var relays []Relay
	old, had := n.g.Profile(n.self.ID())
	if had {
		relays = old.Relays
	}
	if relay != nil {
		if err := relay.Validate(); err != nil {
			return err
		}
		if len(relays) == 0 || relays[0] != *relay {
			if len(relays) > 0 {
				relays = []Relay{*relay, relays[0]}
				if n.f.MigrateUntil.IsZero() {
					n.f.MigrateUntil = n.now().Add(Transition)
				}
			} else {
				relays = []Relay{*relay}
			}
		}
	}
	if had && old.Name == name && old.Platform == platform && old.Inbound == inbound && slices.Equal(old.Relays, relays) {
		return nil
	}
	if _, err := n.g.NewProfile(n.self, name, platform, inbound, relays); err != nil {
		return err
	}
	return n.commitLocked(nil)
}

// Endpoint is one tailcat address this node listens on or dials.
type Endpoint struct {
	tunnel.MemberEndpoint
	// Control is set only for the preferred address (current key, first
	// relay); the others serve sync and catch-up.
	Control bool
}

func (n *Node) endpointsLocked(member ID, syncOnly bool) []Endpoint {
	if n.broken != nil || n.keyErr != nil || !n.g.State().Active(member) {
		return nil
	}
	p, ok := n.g.Profile(member)
	if syncOnly && p.Platform == "android" || !syncOnly && (!ok || !p.Inbound) {
		return nil
	}
	relays := p.Relays
	// Older desktops erased their relay when switched off. Try our own
	// home relay once when we have no route from the peer. Only a signed
	// profile received over authenticated sync can enable its control entry.
	if syncOnly && len(relays) == 0 {
		if self, ok := n.g.Profile(n.self.ID()); ok && len(self.Relays) > 0 {
			relays = self.Relays[:1]
		}
	}
	var out []Endpoint
	for _, v := range []ID{n.f.KeyCurrent, n.f.KeyPrev} {
		k, ok := n.k[v]
		if !ok {
			continue
		}
		for i, r := range relays {
			gid, ver, mid := n.g.ID(), v, member
			out = append(out, Endpoint{
				MemberEndpoint: tunnel.MemberEndpoint{GroupKey: k, GroupID: gid[:], KeyVersion: ver[:], MemberID: mid[:], Relay: r},
				Control:        !syncOnly && !n.departing && v == n.g.State().KeyVersion && i == 0,
			})
		}
	}
	return out
}

// ListenEndpoints are the addresses this desktop serves.
func (n *Node) ListenEndpoints() []Endpoint {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.broken != nil || !n.f.Policy.AllowInbound || n.g == nil {
		return nil
	}
	return n.endpointsLocked(n.self.ID(), false)
}

// DialEndpoints are a member's known addresses, preferred first.
func (n *Node) DialEndpoints(member ID) []Endpoint {
	return n.dialEndpoints(member, false)
}

// SyncEndpoints also checks desktops whose last advertised inbound state was off.
func (n *Node) SyncEndpoints(member ID) []Endpoint {
	return n.dialEndpoints(member, true)
}

func (n *Node) dialEndpoints(member ID, syncOnly bool) []Endpoint {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.g == nil || member == n.self.ID() || (!n.departing && n.usableLocked() != nil) {
		return nil
	}
	return n.endpointsLocked(member, syncOnly)
}

// MemberInfo is one row of the device list.
type MemberInfo struct {
	ID         ID
	Name       string
	Platform   string
	Inbound    bool // false: controller only
	Self       bool
	KeyVersion ID // key version of its latest ack
	Synced     bool
	AckHead    ID
}

// Snapshot is the state the UI shows.
type Snapshot struct {
	Group      ID
	InGroup    bool
	Err        error
	Conflict   bool
	Rebuild    []ID // conflict peers that need local handling
	KeyVersion ID
	HeldKey    ID
	Members    []MemberInfo
	Policy     Policy
}

// Snapshot returns the device list.
func (n *Node) Snapshot() Snapshot {
	n.mu.Lock()
	defer n.mu.Unlock()
	s := Snapshot{Err: n.broken, Policy: n.f.Policy, HeldKey: n.f.KeyCurrent}
	if s.Err == nil {
		s.Err = n.keyErr
	}
	if n.g == nil {
		return s
	}
	st := n.g.State()
	s.Group, s.InGroup, s.KeyVersion = n.g.ID(), st.Active(n.self.ID()), st.KeyVersion
	if c := n.g.Conflict(); c != nil {
		s.Conflict, s.Rebuild = true, n.g.ConflictPeers()
	}
	for id := range st.Members {
		m := MemberInfo{ID: id, Self: id == n.self.ID()}
		if p, ok := n.g.Profile(id); ok {
			m.Name, m.Platform, m.Inbound = p.Name, p.Platform, p.Inbound
		}
		if a, ok := n.g.Ack(id); ok {
			m.KeyVersion, m.AckHead, m.Synced = a.KeyVersion, a.Head, a.Head == st.Head
		}
		if m.Self {
			m.Synced, m.AckHead, m.KeyVersion = true, st.Head, n.f.KeyCurrent
		}
		s.Members = append(s.Members, m)
	}
	slices.SortFunc(s.Members, func(a, b MemberInfo) int { return strings.Compare(a.Name+a.ID.String(), b.Name+b.ID.String()) })
	return s
}

// Progress reports which members have stored a change (e.g. a removal).
func (n *Node) Progress(record ID) (confirmed, pending []ID) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.g == nil {
		return nil, nil
	}
	return n.g.Progress(n.self.ID(), record)
}

// Invited lists members target added, shown when removing it.
func (n *Node) Invited(target ID) []ID {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.g == nil {
		return nil
	}
	return n.g.Invited(target)
}
