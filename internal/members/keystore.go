package members

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
)

// KeyStore holds secrets: the member private key and the group keys, one
// slot per (group, key version). Platform implementations wrap the macOS
// keychain, Windows DPAPI, Android Keystore or Linux Secret Service;
// FileKeyStore is the private-directory fallback.
type KeyStore interface {
	Put(slot string, secret []byte) error
	Get(slot string) ([]byte, error)
	Delete(slot string) error
	List() ([]string, error)
}

// ErrNoSlot means the slot does not exist.
var ErrNoSlot = errors.New("凭据槽不存在")

// Slot names.
const identitySlot = "identity"

func pendingSlot(member ID) string { return "pending/" + member.String() }

// KeySlot names the slot of one group key version.
func KeySlot(group, version ID) string { return "k/" + group.String() + "/" + version.String() }

// FileKeyStore keeps each slot in a 0600 file inside a 0700 directory.
type FileKeyStore struct{ Dir string }

func (f FileKeyStore) path(slot string) string {
	return filepath.Join(f.Dir, b64.EncodeToString([]byte(slot)))
}

func (f FileKeyStore) Put(slot string, secret []byte) error {
	if err := os.MkdirAll(f.Dir, 0o700); err != nil {
		return err
	}
	return writeFileSync(f.path(slot), secret, 0o600)
}

func (f FileKeyStore) Get(slot string) ([]byte, error) {
	b, err := os.ReadFile(f.path(slot))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNoSlot
	}
	return b, err
}

func (f FileKeyStore) Delete(slot string) error {
	err := os.Remove(f.path(slot))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func (f FileKeyStore) List() ([]string, error) {
	es, err := os.ReadDir(f.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range es {
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if n, err := b64.DecodeString(e.Name()); err == nil {
			out = append(out, string(n))
		}
	}
	return out, nil
}

// MemKeyStore is an in-memory KeyStore for tests and the CLI.
type MemKeyStore struct {
	mu sync.Mutex
	m  map[string][]byte
}

func (m *MemKeyStore) Put(slot string, secret []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.m == nil {
		m.m = map[string][]byte{}
	}
	m.m[slot] = slices.Clone(secret)
	return nil
}

func (m *MemKeyStore) Get(slot string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.m[slot]
	if !ok {
		return nil, ErrNoSlot
	}
	return slices.Clone(b), nil
}

func (m *MemKeyStore) Delete(slot string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.m, slot)
	return nil
}

func (m *MemKeyStore) List() ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for k := range m.m {
		out = append(out, k)
	}
	return out, nil
}

// putVerified writes a slot and reads it back.
func putVerified(ks KeyStore, slot string, secret []byte) error {
	if err := ks.Put(slot, secret); err != nil {
		return err
	}
	got, err := ks.Get(slot)
	if err != nil || string(got) != string(secret) {
		return fmt.Errorf("凭据槽 %s 写入后读取校验失败", slot)
	}
	return nil
}

// writeFileSync writes via a temporary file, fsyncs, renames and fsyncs the
// directory.
func writeFileSync(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(perm); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	if d, err := os.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}
	return nil
}
