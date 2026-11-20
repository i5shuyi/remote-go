// Package members implements "my devices": a small group of a person's
// own devices that connect to each other without device codes.
//
// Membership is a signed, linear history starting at a self-signed genesis
// record. Every member may append changes (add, remove, rotate the group
// key K). Devices merge histories by prefix; two valid children of the same
// parent are a fork, which stops member control until the person rebuilds
// the group (docs/design/my-devices.md).
package members

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"

	"github.com/i5shuyi/remote-go/internal/tunnel"
)

// Relay is a normalized relay descriptor (see tunnel.MemberRelay).
type Relay = tunnel.MemberRelay

// Version is the member protocol version carried in every record body.
const Version = 1

// Record types. Each has its own signing prefix.
const (
	TypeGenesis = "genesis"
	TypeChange  = "change"
	TypeIntent  = "intent"
	TypeProfile = "profile"
	TypeAck     = "ack"
	TypeReceipt = "receipt"
)

var knownTypes = map[string]bool{TypeGenesis: true, TypeChange: true, TypeIntent: true, TypeProfile: true, TypeAck: true, TypeReceipt: true}

// Change operations.
const (
	OpAdd          = "add"
	OpRemove       = "remove"
	OpRotate       = "rotate"
	OpRemoveRotate = "remove_rotate"
)

var b64 = base64.RawURLEncoding.Strict()

// B64 is binary data encoded as unpadded base64url.
type B64 []byte

func (b B64) MarshalJSON() ([]byte, error) { return json.Marshal(b64.EncodeToString(b)) }

func (b *B64) UnmarshalJSON(j []byte) error {
	var s string
	if err := json.Unmarshal(j, &s); err != nil {
		return err
	}
	d, err := b64.DecodeString(s)
	if err != nil {
		return fmt.Errorf("base64url 无效：%w", err)
	}
	*b = d
	return nil
}

// ID is a SHA-256 value: a record ID, member ID, group ID or key version.
type ID [32]byte

func (id ID) MarshalJSON() ([]byte, error) { return json.Marshal(b64.EncodeToString(id[:])) }

func (id *ID) UnmarshalJSON(j []byte) error {
	var b B64
	if err := b.UnmarshalJSON(j); err != nil {
		return err
	}
	if len(b) != 32 {
		return errors.New("ID 长度应为 32 字节")
	}
	copy(id[:], b)
	return nil
}

func (id ID) String() string { return b64.EncodeToString(id[:]) }

// Short is the abbreviated form for display only.
func (id ID) Short() string { return fmt.Sprintf("%x", id[:4]) }

func (id ID) IsZero() bool { return id == ID{} }

// MemberID is the full SHA-256 of an Ed25519 public key.
func MemberID(pub ed25519.PublicKey) ID { return sha256.Sum256(pub) }

// KeyFingerprint is the SHA-256 of a group key.
func KeyFingerprint(k []byte) ID { return sha256.Sum256(k) }

// Signed is a record on the wire: the canonical body, its type and a
// signature over the type prefix and body.
type Signed struct {
	Type string          `json:"type"`
	Body json.RawMessage `json:"body"`
	Sig  B64             `json:"sig"`
}

// SigningBytes is "remote-go/members/v1/<type>" ‖ 0x00 ‖ canonical body.
func SigningBytes(typ string, body []byte) []byte {
	b := make([]byte, 0, 32+len(typ)+len(body))
	b = append(b, "remote-go/members/v1/"+typ...)
	b = append(b, 0)
	return append(b, body...)
}

// ID is the SHA-256 of the record's signing bytes.
func (s Signed) ID() ID { return sha256.Sum256(SigningBytes(s.Type, s.Body)) }

// Equal reports whether two records are byte-identical.
func (s Signed) Equal(o Signed) bool {
	return s.Type == o.Type && bytes.Equal(s.Body, o.Body) && bytes.Equal(s.Sig, o.Sig)
}

func sign(priv ed25519.PrivateKey, typ string, body any) (Signed, error) {
	c, err := marshalCanon(body)
	if err != nil {
		return Signed{}, err
	}
	return Signed{Type: typ, Body: c, Sig: ed25519.Sign(priv, SigningBytes(typ, c))}, nil
}

// open checks the envelope and decodes the body into v. The signature is
// checked separately, once the signer's key is known.
func (s Signed) open(typ string, v any) error {
	if !knownTypes[s.Type] {
		return fmt.Errorf("未知记录类型 %q", s.Type)
	}
	if s.Type != typ {
		return fmt.Errorf("记录类型应为 %s，收到 %s", typ, s.Type)
	}
	if len(s.Sig) != ed25519.SignatureSize {
		return errors.New("签名长度无效")
	}
	return decodeStrict(s.Body, v)
}

func (s Signed) verify(pub ed25519.PublicKey) error {
	if len(pub) != ed25519.PublicKeySize || !ed25519.Verify(pub, SigningBytes(s.Type, s.Body), s.Sig) {
		return errors.New("签名无效")
	}
	return nil
}

// Genesis creates the group. Its record ID is the group ID.
type Genesis struct {
	V       int `json:"v"`
	Seed    B64 `json:"seed"`
	Creator B64 `json:"creator"`
	KeyFP   ID  `json:"key_fp"`
}

// Change is one membership change appended to the history.
type Change struct {
	V      int        `json:"v"`
	Group  ID         `json:"group"`
	Parent ID         `json:"parent"`
	Issuer ID         `json:"issuer"`
	Op     string     `json:"op"`
	Add    *AddParams `json:"add,omitempty"`
	Target *ID        `json:"target,omitempty"`
	KeyFP  *ID        `json:"key_fp,omitempty"`
}

// AddParams binds the new member's key and its own signed intent to join.
type AddParams struct {
	Key    B64    `json:"key"`
	Member ID     `json:"member"`
	Intent Signed `json:"intent"`
}

// Intent is signed by the joining identity.
type Intent struct {
	V       int `json:"v"`
	Group   ID  `json:"group"`
	Key     B64 `json:"key"`
	Inviter ID  `json:"inviter"`
	Txn     ID  `json:"txn"`
}

// Profile is a member's self-description: discovery information only.
type Profile struct {
	V        int     `json:"v"`
	Group    ID      `json:"group"`
	Member   ID      `json:"member"`
	Seq      string  `json:"seq"`
	Name     string  `json:"name"`
	Platform string  `json:"platform"`
	Inbound  bool    `json:"inbound"` // false: controller only (Android)
	Relays   []Relay `json:"relays"`
}

// Ack states what a member has durably stored. A zero KeyVersion means
// the member has the signed history but needs the current key delivered.
type Ack struct {
	V          int `json:"v"`
	Group      ID  `json:"group"`
	Member     ID  `json:"member"`
	Head       ID  `json:"head"`
	KeyVersion ID  `json:"key_version"`
}

// Receipt confirms that Signer received Target's profile record.
type Receipt struct {
	V       int `json:"v"`
	Group   ID  `json:"group"`
	Signer  ID  `json:"signer"`
	Target  ID  `json:"target"`
	Profile ID  `json:"profile"`
}

var seqRE = regexp.MustCompile(`^(0|[1-9][0-9]{0,18})$`)

func parseSeq(s string) (uint64, error) {
	if !seqRE.MatchString(s) {
		return 0, fmt.Errorf("序号 %q 无效", s)
	}
	return strconv.ParseUint(s, 10, 64)
}

// ParseSigned decodes a record received from elsewhere, checking only the
// envelope. Callers validate the body against their state.
func ParseSigned(b []byte) (Signed, error) {
	var s Signed
	err := json.Unmarshal(b, &s)
	return s, err
}

// UnmarshalJSON makes every decoded record go through the strict checks.
func (s *Signed) UnmarshalJSON(b []byte) error {
	type plain Signed
	var p plain
	if _, err := canonicalize(b); err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(&p); err != nil {
		return err
	}
	if !knownTypes[p.Type] {
		return fmt.Errorf("未知记录类型 %q", p.Type)
	}
	c, err := canonicalize(p.Body)
	if err != nil || !bytes.Equal(c, p.Body) {
		return errors.New("记录体不是规范 JSON")
	}
	*s = Signed(p)
	return nil
}

// ParseID decodes the base64url form of an ID.
func ParseID(s string) (ID, error) {
	var id ID
	err := id.UnmarshalJSON([]byte(`"` + s + `"`))
	return id, err
}
