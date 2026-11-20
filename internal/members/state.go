package members

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"maps"
)

// State is the membership in effect at one point of the history.
type State struct {
	Group      ID
	Head       ID
	Len        int // changes applied after genesis
	Members    map[ID]ed25519.PublicKey
	Joined     map[ID]bool // every identity ever added, including removed ones
	Removed    map[ID]bool // tombstones
	InvitedBy  map[ID]ID
	KeyVersion ID
	KeyFP      ID
}

// Active reports whether id is a current member.
func (s *State) Active(id ID) bool { _, ok := s.Members[id]; return ok }

func (s *State) clone() *State {
	c := *s
	c.Members = maps.Clone(s.Members)
	c.Joined = maps.Clone(s.Joined)
	c.Removed = maps.Clone(s.Removed)
	c.InvitedBy = maps.Clone(s.InvitedBy)
	return &c
}

func checkV(v int) error {
	if v != Version {
		return fmt.Errorf("不支持的成员协议版本 %d", v)
	}
	return nil
}

// openGenesis validates a self-signed genesis record.
func openGenesis(g Signed) (*State, error) {
	var b Genesis
	if err := g.open(TypeGenesis, &b); err != nil {
		return nil, err
	}
	if err := checkV(b.V); err != nil {
		return nil, err
	}
	if len(b.Seed) != 32 || len(b.Creator) != ed25519.PublicKeySize {
		return nil, errors.New("创组记录字段长度无效")
	}
	if err := g.verify(ed25519.PublicKey(b.Creator)); err != nil {
		return nil, err
	}
	id := g.ID()
	creator := MemberID(ed25519.PublicKey(b.Creator))
	return &State{
		Group:      id,
		Head:       id,
		Members:    map[ID]ed25519.PublicKey{creator: ed25519.PublicKey(b.Creator)},
		Joined:     map[ID]bool{creator: true},
		Removed:    map[ID]bool{},
		InvitedBy:  map[ID]ID{},
		KeyVersion: id,
		KeyFP:      b.KeyFP,
	}, nil
}

// ErrNotChild means the change does not extend this state's head.
var ErrNotChild = errors.New("父记录不是当前历史头")

// check validates c as the next change after s without modifying s.
func (s *State) check(c Signed) (*Change, error) {
	var b Change
	if err := c.open(TypeChange, &b); err != nil {
		return nil, err
	}
	if err := checkV(b.V); err != nil {
		return nil, err
	}
	if b.Group != s.Group {
		return nil, errors.New("记录属于其他组")
	}
	if b.Parent != s.Head {
		return nil, ErrNotChild
	}
	pub, ok := s.Members[b.Issuer]
	if !ok {
		return nil, errors.New("签发者在父记录处不是有效成员")
	}
	if err := c.verify(pub); err != nil {
		return nil, err
	}
	hasAdd, hasTarget, hasFP := b.Add != nil, b.Target != nil, b.KeyFP != nil
	switch b.Op {
	case OpAdd:
		if !hasAdd || hasTarget || hasFP {
			return nil, errors.New("添加记录字段无效")
		}
		a := b.Add
		if len(a.Key) != ed25519.PublicKeySize || MemberID(ed25519.PublicKey(a.Key)) != a.Member {
			return nil, errors.New("成员 ID 与公钥不符")
		}
		if s.Joined[a.Member] {
			return nil, errors.New("同一身份只能加入一次")
		}
		var in Intent
		if err := a.Intent.open(TypeIntent, &in); err != nil {
			return nil, fmt.Errorf("入组意愿：%w", err)
		}
		if err := checkV(in.V); err != nil {
			return nil, err
		}
		if in.Group != s.Group || in.Inviter != b.Issuer || string(in.Key) != string(a.Key) {
			return nil, errors.New("入组意愿与添加记录不符")
		}
		if err := a.Intent.verify(ed25519.PublicKey(a.Key)); err != nil {
			return nil, fmt.Errorf("入组意愿：%w", err)
		}
	case OpRemove, OpRemoveRotate:
		if hasAdd || !hasTarget || hasFP != (b.Op == OpRemoveRotate) {
			return nil, errors.New("移除记录字段无效")
		}
		if !s.Active(*b.Target) {
			return nil, errors.New("移除对象不是有效成员")
		}
	case OpRotate:
		if hasAdd || hasTarget || !hasFP {
			return nil, errors.New("轮换记录字段无效")
		}
	default:
		return nil, fmt.Errorf("未知操作 %q", b.Op)
	}
	return &b, nil
}

// apply validates c and returns the state after it.
func (s *State) apply(c Signed) (*State, *Change, error) {
	b, err := s.check(c)
	if err != nil {
		return nil, nil, err
	}
	n := s.clone()
	id := c.ID()
	n.Head = id
	n.Len++
	switch b.Op {
	case OpAdd:
		n.Members[b.Add.Member] = ed25519.PublicKey(b.Add.Key)
		n.Joined[b.Add.Member] = true
		n.InvitedBy[b.Add.Member] = b.Issuer
	case OpRemove, OpRemoveRotate:
		delete(n.Members, *b.Target)
		n.Removed[*b.Target] = true
	}
	if b.KeyFP != nil {
		n.KeyVersion = id
		n.KeyFP = *b.KeyFP
	}
	return n, b, nil
}
