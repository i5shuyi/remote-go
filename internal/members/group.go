package members

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"slices"
	"strconv"
)

var (
	// ErrFork means two valid, conflicting changes share a parent. The
	// group has entered the conflict state.
	ErrFork = errors.New("成员历史分叉")
	// ErrConflict refuses an operation while the group is in conflict.
	ErrConflict = errors.New("成员历史存在冲突，需要重建组")
	// ErrUnknownParent asks the caller to fetch earlier records first.
	ErrUnknownParent = errors.New("未知父记录")
	// ErrNotMember means this device is no longer an active member.
	ErrNotMember = errors.New("本机不是有效成员")
)

// Conflict is persisted fork evidence: the two valid children of Parent.
// Theirs holds the verified chain of the other branch as far as received.
type Conflict struct {
	Parent ID       `json:"parent"`
	Ours   Signed   `json:"ours"`
	Theirs []Signed `json:"theirs"`
}

// Group is one device's view of a group. It is not safe for concurrent
// use; Node serializes access.
type Group struct {
	genesis  Signed
	history  []Signed
	states   []*State // states[i] is the state after i changes
	profiles map[ID]Signed
	acks     map[ID]Signed
	receipts map[[2]ID]Signed // (signer, target)
	conflict *Conflict
}

// Identity is this device's member key.
type Identity struct {
	Priv ed25519.PrivateKey
}

func (id Identity) Public() ed25519.PublicKey { return id.Priv.Public().(ed25519.PublicKey) }
func (id Identity) ID() ID                    { return MemberID(id.Public()) }

// NewIdentity generates a fresh member key; each join uses a new one.
func NewIdentity() Identity {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	return Identity{Priv: priv}
}

// NewGroupKey returns a fresh random 32-byte group key.
func NewGroupKey() []byte {
	k := make([]byte, 32)
	rand.Read(k)
	return k
}

// Create starts a group with creator as its only member.
func Create(creator Identity, k []byte) (*Group, error) {
	seed := make([]byte, 32)
	rand.Read(seed)
	g, err := sign(creator.Priv, TypeGenesis, Genesis{V: Version, Seed: seed, Creator: B64(creator.Public()), KeyFP: KeyFingerprint(k)})
	if err != nil {
		return nil, err
	}
	return Load(g, nil)
}

// Load rebuilds a group from its genesis and change history, validating
// every record.
func Load(genesis Signed, history []Signed) (*Group, error) {
	s, err := openGenesis(genesis)
	if err != nil {
		return nil, fmt.Errorf("创组记录：%w", err)
	}
	g := &Group{genesis: genesis, states: []*State{s}, profiles: map[ID]Signed{}, acks: map[ID]Signed{}, receipts: map[[2]ID]Signed{}}
	for i, c := range history {
		n, _, err := g.state().apply(c)
		if err != nil {
			return nil, fmt.Errorf("第 %d 条成员记录：%w", i+1, err)
		}
		g.history = append(g.history, c)
		g.states = append(g.states, n)
	}
	return g, nil
}

func (g *Group) state() *State { return g.states[len(g.states)-1] }

// State returns the current membership. Callers must not modify it.
func (g *Group) State() *State       { return g.state() }
func (g *Group) ID() ID              { return g.state().Group }
func (g *Group) Head() ID            { return g.state().Head }
func (g *Group) Genesis() Signed     { return g.genesis }
func (g *Group) History() []Signed   { return slices.Clone(g.history) }
func (g *Group) Conflict() *Conflict { return g.conflict }

// index returns how many changes lead to id (0 for genesis), or -1.
func (g *Group) index(id ID) int {
	for i, s := range g.states {
		if s.Head == id {
			return i
		}
	}
	return -1
}

// Knows reports whether head is on this device's history.
func (g *Group) Knows(head ID) bool { return g.index(head) >= 0 }

// Suffix returns the changes after head, or the whole history when head is
// unknown (the peer is ahead of us or forked).
func (g *Group) Suffix(head ID) []Signed {
	if i := g.index(head); i >= 0 {
		return slices.Clone(g.history[i:])
	}
	return g.History()
}

// ApplyResult reports what Apply changed.
type ApplyResult struct {
	Added   int
	Removed []ID // members that lost membership
}

// Apply merges a chain of changes. Records already present are skipped;
// the missing suffix is committed atomically. A valid change that
// conflicts with a local one records fork evidence and returns ErrFork.
// An invalid record rejects the whole message and changes nothing.
func (g *Group) Apply(records []Signed) (ApplyResult, error) {
	var res ApplyResult
	if g.conflict != nil {
		return res, ErrConflict
	}
	pos := -1 // position in our history the incoming chain has reached
	var added []Signed
	var states []*State
	for _, r := range records {
		var b Change
		if err := r.open(TypeChange, &b); err != nil {
			return ApplyResult{}, err
		}
		if pos < 0 {
			pos = g.index(b.Parent)
			if pos < 0 {
				return ApplyResult{}, ErrUnknownParent
			}
		}
		if len(added) == 0 && pos < len(g.history) {
			if g.history[pos].Equal(r) {
				pos++
				continue
			}
			// A different child of a parent we already extended.
			if _, _, err := g.states[pos].apply(r); err != nil {
				return ApplyResult{}, err
			}
			return ApplyResult{}, g.fork(pos, records[slices.IndexFunc(records, r.Equal):])
		}
		cur := g.state()
		if len(states) > 0 {
			cur = states[len(states)-1]
		}
		n, _, err := cur.apply(r)
		if err != nil {
			return ApplyResult{}, err
		}
		added = append(added, r)
		states = append(states, n)
	}
	if len(added) == 0 {
		return res, nil
	}
	before := g.state()
	g.history = append(g.history, added...)
	g.states = append(g.states, states...)
	for id := range before.Members {
		if !g.state().Active(id) {
			res.Removed = append(res.Removed, id)
		}
	}
	res.Added = len(added)
	g.prune()
	return res, nil
}

// fork records evidence: ours is history[pos], theirs starts with chain[0].
func (g *Group) fork(pos int, chain []Signed) error {
	s := g.states[pos]
	var theirs []Signed
	for _, r := range chain {
		n, _, err := s.apply(r)
		if err != nil {
			break
		}
		theirs = append(theirs, r)
		s = n
	}
	g.conflict = &Conflict{Parent: g.states[pos].Head, Ours: g.history[pos], Theirs: theirs}
	return ErrFork
}

// ApplyConflict accepts fork evidence relayed by another member. Both
// children must be valid at a parent on our history.
func (g *Group) ApplyConflict(c Conflict) error {
	if g.conflict != nil {
		return nil
	}
	pos := g.index(c.Parent)
	if pos < 0 || len(c.Theirs) == 0 {
		return ErrUnknownParent
	}
	s := g.states[pos]
	if c.Ours.Equal(c.Theirs[0]) {
		return errors.New("冲突证据中的两条记录相同")
	}
	for _, r := range []Signed{c.Ours, c.Theirs[0]} {
		if _, _, err := s.apply(r); err != nil {
			return err
		}
	}
	if pos == len(g.history) {
		g.conflict = &Conflict{Parent: c.Parent, Ours: c.Ours, Theirs: c.Theirs[:1]}
		return ErrFork
	}
	if g.history[pos].Equal(c.Ours) {
		return g.fork(pos, c.Theirs)
	}
	return g.fork(pos, []Signed{c.Ours})
}

// ConflictPeers are the members who may exchange history and evidence
// during a conflict: valid at the fork point, removed on no known branch.
func (g *Group) ConflictPeers() []ID {
	if g.conflict == nil {
		return nil
	}
	pos := g.index(g.conflict.Parent)
	base := g.states[pos]
	removed := map[ID]bool{}
	mark := func(s *State, chain []Signed) {
		for _, r := range chain {
			n, _, err := s.apply(r)
			if err != nil {
				return
			}
			for id := range n.Removed {
				removed[id] = true
			}
			s = n
		}
	}
	mark(base, g.history[pos:])
	mark(base, []Signed{g.conflict.Ours})
	mark(base, g.conflict.Theirs)
	var out []ID
	for id := range base.Members {
		if !removed[id] {
			out = append(out, id)
		}
	}
	slices.SortFunc(out, func(a, b ID) int { return bytes.Compare(a[:], b[:]) })
	return out
}

func (g *Group) restoreConflict(c *Conflict) error {
	if c == nil {
		return nil
	}
	if err := g.ApplyConflict(*c); !errors.Is(err, ErrFork) && err != nil {
		return err
	}
	return nil
}

// newChange signs and applies a local change.
func (g *Group) newChange(self Identity, op string, set func(*Change)) (Signed, ApplyResult, error) {
	if g.conflict != nil {
		return Signed{}, ApplyResult{}, ErrConflict
	}
	s := g.state()
	if !s.Active(self.ID()) {
		return Signed{}, ApplyResult{}, ErrNotMember
	}
	b := Change{V: Version, Group: s.Group, Parent: s.Head, Issuer: self.ID(), Op: op}
	set(&b)
	r, err := sign(self.Priv, TypeChange, b)
	if err != nil {
		return Signed{}, ApplyResult{}, err
	}
	res, err := g.Apply([]Signed{r})
	return r, res, err
}

// NewIntent is signed by a joining identity for one pairing transaction.
func NewIntent(joiner Identity, group, inviter, txn ID) (Signed, error) {
	return sign(joiner.Priv, TypeIntent, Intent{V: Version, Group: group, Key: B64(joiner.Public()), Inviter: inviter, Txn: txn})
}

// Add appends the member that signed intent.
func (g *Group) Add(self Identity, intent Signed) (Signed, error) {
	var in Intent
	if err := intent.open(TypeIntent, &in); err != nil {
		return Signed{}, err
	}
	r, _, err := g.newChange(self, OpAdd, func(c *Change) {
		c.Add = &AddParams{Key: in.Key, Member: MemberID(ed25519.PublicKey(in.Key)), Intent: intent}
	})
	return r, err
}

// Remove removes target; with newKey it also rotates the group key in the
// same change.
func (g *Group) Remove(self Identity, target ID, newKey []byte) (Signed, ApplyResult, error) {
	op := OpRemove
	if newKey != nil {
		op = OpRemoveRotate
	}
	return g.newChange(self, op, func(c *Change) {
		c.Target = &target
		if newKey != nil {
			fp := KeyFingerprint(newKey)
			c.KeyFP = &fp
		}
	})
}

// Rotate replaces the group key.
func (g *Group) Rotate(self Identity, newKey []byte) (Signed, error) {
	fp := KeyFingerprint(newKey)
	r, _, err := g.newChange(self, OpRotate, func(c *Change) { c.KeyFP = &fp })
	return r, err
}

// AddRecord finds the change that added member, if any.
func (g *Group) AddRecord(member ID) (Signed, int, bool) {
	for i, r := range g.history {
		var b Change
		if r.open(TypeChange, &b) == nil && b.Op == OpAdd && b.Add.Member == member {
			return r, i + 1, true
		}
	}
	return Signed{}, 0, false
}

// Invited lists the members target added, for review when removing it.
func (g *Group) Invited(target ID) []ID {
	var out []ID
	for id, by := range g.state().InvitedBy {
		if by == target && g.state().Active(id) {
			out = append(out, id)
		}
	}
	return out
}

// KeyVersionFP returns the fingerprint committed for a key version on the
// current history.
func (g *Group) KeyVersionFP(version ID) (ID, bool) {
	for _, s := range g.states {
		if s.KeyVersion == version {
			return s.KeyFP, true
		}
	}
	return ID{}, false
}

// keyIndex is the history position that introduced version, or -1.
func (g *Group) keyIndex(version ID) int {
	for i, s := range g.states {
		if s.KeyVersion == version {
			return i
		}
	}
	return -1
}

// --- profiles ---

// ApplyProfile keeps the newest valid profile of each active member.
func (g *Group) ApplyProfile(r Signed) (bool, error) {
	var p Profile
	if err := r.open(TypeProfile, &p); err != nil {
		return false, err
	}
	if err := checkV(p.V); err != nil {
		return false, err
	}
	seq, err := parseSeq(p.Seq)
	if err != nil {
		return false, err
	}
	if p.Group != g.ID() {
		return false, errors.New("自述属于其他组")
	}
	pub, ok := g.state().Members[p.Member]
	if !ok {
		return false, errors.New("自述签发者不是有效成员")
	}
	if len(p.Relays) > 2 {
		return false, errors.New("中继候选过多")
	}
	for _, rl := range p.Relays {
		if err := rl.Validate(); err != nil {
			return false, err
		}
	}
	if err := r.verify(pub); err != nil {
		return false, err
	}
	if old, ok := g.profiles[p.Member]; ok {
		op, _ := g.Profile(p.Member)
		oseq, _ := parseSeq(op.Seq)
		oid, nid := old.ID(), r.ID()
		if seq < oseq || seq == oseq && bytes.Compare(nid[:], oid[:]) <= 0 {
			return false, nil
		}
	}
	g.profiles[p.Member] = r
	return true, nil
}

// Profile returns a member's current profile.
func (g *Group) Profile(member ID) (Profile, bool) {
	r, ok := g.profiles[member]
	if !ok {
		return Profile{}, false
	}
	var p Profile
	r.open(TypeProfile, &p)
	return p, true
}

// ProfileRecord returns the signed profile, for passing on.
func (g *Group) ProfileRecord(member ID) (Signed, bool) { r, ok := g.profiles[member]; return r, ok }

// NewProfile signs this device's profile with the next sequence number.
func (g *Group) NewProfile(self Identity, name, platform string, inbound bool, relays []Relay) (Signed, error) {
	var seq uint64
	if p, ok := g.Profile(self.ID()); ok {
		seq, _ = parseSeq(p.Seq)
		seq++
	}
	r, err := sign(self.Priv, TypeProfile, Profile{V: Version, Group: g.ID(), Member: self.ID(), Seq: strconv.FormatUint(seq, 10),
		Name: name, Platform: platform, Inbound: inbound, Relays: relays})
	if err != nil {
		return Signed{}, err
	}
	if _, err := g.ApplyProfile(r); err != nil {
		return Signed{}, err
	}
	return r, nil
}

// --- acks ---

// NewAck states this device's durable head and the key version it holds.
func (g *Group) NewAck(self Identity, held ID) (Signed, error) {
	return sign(self.Priv, TypeAck, Ack{V: Version, Group: g.ID(), Member: self.ID(), Head: g.Head(), KeyVersion: held})
}

// ErrUnknownHead means an ack points past our history: sync first.
var ErrUnknownHead = errors.New("确认指向未知历史")

// ApplyAck keeps the furthest compatible ack of each member.
func (g *Group) ApplyAck(r Signed) (Ack, error) {
	var a Ack
	if err := r.open(TypeAck, &a); err != nil {
		return a, err
	}
	if err := checkV(a.V); err != nil {
		return a, err
	}
	if a.Group != g.ID() {
		return a, errors.New("确认属于其他组")
	}
	at := g.index(a.Head)
	if at < 0 {
		return a, ErrUnknownHead
	}
	pub, ok := g.states[at].Members[a.Member]
	if !ok {
		return a, errors.New("确认者在该历史处不是成员")
	}
	if k := g.keyIndex(a.KeyVersion); k < 0 || k > at {
		return a, errors.New("确认的密钥版本与历史不相容")
	}
	if err := r.verify(pub); err != nil {
		return a, err
	}
	if old, ok := g.Ack(a.Member); ok {
		oat := g.index(old.Head)
		if at < oat || at == oat && g.keyIndex(a.KeyVersion) <= g.keyIndex(old.KeyVersion) {
			return a, nil
		}
	}
	g.acks[a.Member] = r
	return a, nil
}

// Ack returns a member's latest stored ack.
func (g *Group) Ack(member ID) (Ack, bool) {
	r, ok := g.acks[member]
	if !ok {
		return Ack{}, false
	}
	var a Ack
	r.open(TypeAck, &a)
	return a, true
}

// Progress reports which active members have durably stored record (by
// signed ack). self is always counted as confirmed.
func (g *Group) Progress(self ID, record ID) (confirmed, pending []ID) {
	at := g.index(record)
	for id := range g.state().Members {
		a, ok := g.Ack(id)
		if id == self || ok && at >= 0 && g.index(a.Head) >= at {
			confirmed = append(confirmed, id)
		} else {
			pending = append(pending, id)
		}
	}
	return
}

// KeySignedOff reports whether every active member holds version or a
// later key.
func (g *Group) KeySignedOff(self ID, version ID) bool {
	k := g.keyIndex(version)
	for id := range g.state().Members {
		if id == self {
			continue
		}
		a, ok := g.Ack(id)
		if !ok || g.keyIndex(a.KeyVersion) < k {
			return false
		}
	}
	return k >= 0
}

// --- receipts ---

// NewReceipt confirms target's current profile.
func (g *Group) NewReceipt(self Identity, target ID) (Signed, error) {
	pr, ok := g.profiles[target]
	if !ok {
		return Signed{}, errors.New("没有该成员的自述")
	}
	r, err := sign(self.Priv, TypeReceipt, Receipt{V: Version, Group: g.ID(), Signer: self.ID(), Target: target, Profile: pr.ID()})
	if err != nil {
		return Signed{}, err
	}
	_, err = g.ApplyReceipt(r)
	return r, err
}

// ApplyReceipt stores a receipt signed by an active member.
func (g *Group) ApplyReceipt(r Signed) (bool, error) {
	var b Receipt
	if err := r.open(TypeReceipt, &b); err != nil {
		return false, err
	}
	if err := checkV(b.V); err != nil {
		return false, err
	}
	if b.Group != g.ID() {
		return false, errors.New("回执属于其他组")
	}
	pub, ok := g.state().Members[b.Signer]
	if !ok {
		return false, errors.New("回执签发者不是有效成员")
	}
	if err := r.verify(pub); err != nil {
		return false, err
	}
	k := [2]ID{b.Signer, b.Target}
	if old, ok := g.receipts[k]; ok && old.Equal(r) {
		return false, nil
	}
	g.receipts[k] = r
	return true, nil
}

// MigrationDone reports whether every other active member has a receipt
// for target's current profile, and that profile prefers relay.
func (g *Group) MigrationDone(target ID, relay Relay) bool {
	pr, ok := g.profiles[target]
	p, _ := g.Profile(target)
	if !ok || len(p.Relays) == 0 || p.Relays[0] != relay {
		return false
	}
	want := pr.ID()
	for id := range g.state().Members {
		if id == target {
			continue
		}
		r, ok := g.receipts[[2]ID{id, target}]
		var b Receipt
		if !ok || r.open(TypeReceipt, &b) != nil || b.Profile != want {
			return false
		}
	}
	return true
}

// prune drops discovery records of members that are no longer active.
func (g *Group) prune() {
	s := g.state()
	for id := range g.profiles {
		if !s.Active(id) {
			delete(g.profiles, id)
		}
	}
	for id := range g.acks {
		if !s.Active(id) {
			delete(g.acks, id)
		}
	}
	for k := range g.receipts {
		if !s.Active(k[0]) || !s.Active(k[1]) {
			delete(g.receipts, k)
		}
	}
}

// Extras returns every stored profile, ack and receipt, for sync.
func (g *Group) Extras() (profiles, acks, receipts []Signed) {
	for _, r := range g.profiles {
		profiles = append(profiles, r)
	}
	for _, r := range g.acks {
		acks = append(acks, r)
	}
	for _, r := range g.receipts {
		receipts = append(receipts, r)
	}
	return
}
