package members

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// ErrLocked means another process owns the member state.
var ErrLocked = errors.New("成员状态正由其他进程写入")

// Policy is the member access policy, committed with the member state so
// one file decides authorization.
type Policy struct {
	AllowInbound bool `json:"allowInbound"` // "允许我的设备直连"
	Unattended   bool `json:"unattended"`   // control without local approval
	ViewOnly     bool `json:"viewOnly"`     // member sessions may watch, not control
}

// PairResult remembers a committed pairing so retries are idempotent.
type PairResult struct {
	Txn    ID `json:"txn"`
	Member ID `json:"member"`
	Record ID `json:"record"`
}

// PendingJoin is a joiner's half-finished pairing.
type PendingJoin struct {
	Member ID `json:"member"`
	Txn    ID `json:"txn,omitzero"`
	Group  ID `json:"group,omitzero"`
}

// File is members.json.
type File struct {
	V          int          `json:"v"`
	Self       ID           `json:"self,omitzero"`
	Genesis    *Signed      `json:"genesis,omitempty"`
	History    []Signed     `json:"history,omitempty"`
	Profiles   []Signed     `json:"profiles,omitempty"`
	Acks       []Signed     `json:"acks,omitempty"`
	Receipts   []Signed     `json:"receipts,omitempty"`
	Conflict   *Conflict    `json:"conflict,omitempty"`
	Pairings   []PairResult `json:"pairings,omitempty"`
	Pending    *PendingJoin `json:"pending,omitempty"`
	KeyCurrent ID           `json:"keyCurrent,omitzero"` // version held as current
	KeyPrev    ID           `json:"keyPrev,omitzero"`    // previous version kept for catch-up
	PrevUntil  time.Time    `json:"prevUntil,omitzero"`
	// MigrateUntil ends serving the old relay candidate.
	MigrateUntil time.Time `json:"migrateUntil,omitzero"`
	Policy       Policy    `json:"policy"`
	// Left lists groups this device stopped using locally (conflict rebuild).
	Left []ID `json:"left,omitempty"`
}

// Store owns members.json in one configuration directory. Only one Store
// per directory can be open, across processes.
type Store struct {
	dir  string
	lock *os.File
}

// OpenStore takes exclusive ownership of the member state in dir.
func OpenStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "members.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := lockFile(f); err != nil {
		f.Close()
		return nil, err
	}
	return &Store{dir: dir, lock: f}, nil
}

func (s *Store) path() string { return filepath.Join(s.dir, "members.json") }

// Load reads the state; a missing file is an empty state.
func (s *Store) Load() (*File, error) {
	b, err := os.ReadFile(s.path())
	if errors.Is(err, os.ErrNotExist) {
		return &File{V: Version}, nil
	}
	if err != nil {
		return nil, err
	}
	var f File
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, err
	}
	if f.V != Version {
		return nil, errors.New("members.json 版本不受支持")
	}
	return &f, nil
}

// Save atomically replaces the state and makes it durable.
func (s *Store) Save(f *File) error {
	b, err := marshalWire(f)
	if err != nil {
		return err
	}
	return writeFileSync(s.path(), b, 0o600)
}

// Close releases ownership.
func (s *Store) Close() error { return s.lock.Close() }
