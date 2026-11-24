package schedule

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
)

// Store persists node artifacts by idempotency key. A directory-backed store
// survives process interruption, which is what makes a resumed run reproduce
// identical segment summaries and output hashes.
type Store struct {
	dir string
	mu  sync.Mutex
	mem map[string][]byte
}

// NewFileStore opens (or creates) a directory-backed store under dir.
func NewFileStore(dir string) (*Store, error) {
	if dir == "" {
		return nil, errors.New("schedule: empty store directory")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Store{dir: dir, mem: map[string][]byte{}}, nil
}

// NewMemoryStore builds an in-process store used by unit tests.
func NewMemoryStore() *Store {
	return &Store{mem: map[string][]byte{}}
}

func (s *Store) path(key string) string {
	sum := sha256.Sum256([]byte(key))
	name := hex.EncodeToString(sum[:])
	return filepath.Join(s.dir, name+".art")
}

func (s *Store) put(key string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mem[key] = append([]byte(nil), data...)
	if s.dir == "" {
		return nil
	}
	tmp := s.path(key) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path(key)) // atomic publish
}

func (s *Store) get(key string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if data, ok := s.mem[key]; ok {
		return append([]byte(nil), data...), true
	}
	if s.dir == "" {
		return nil, false
	}
	data, err := os.ReadFile(s.path(key))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, false
		}
		return nil, false
	}
	s.mem[key] = append([]byte(nil), data...)
	return data, true
}

// Hash returns the SHA-256 of a stored artifact, or empty string if absent.
func (s *Store) Hash(key string) string {
	data, ok := s.get(key)
	if !ok {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
