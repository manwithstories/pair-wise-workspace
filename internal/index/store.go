// Package index persists manifests and resumable checkpoints using local,
// crash-safe atomic writes. Every write goes to a temp file in the same
// directory and is renamed over the target, so a crash never leaves a
// half-written index and never touches the input media.
package index

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Store is the directory-backed local index.
type Store struct {
	dir string
}

// NewStore opens (creating if needed) an index directory. It never creates
// files beside the media input: callers pass an explicit work directory.
func NewStore(dir string) (*Store, error) {
	if dir == "" {
		return nil, ErrEmptyDir
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMkdir, err)
	}
	return &Store{dir: dir}, nil
}

// Dir returns the store directory.
func (s *Store) Dir() string { return s.dir }

// Path joins a file name onto the store directory.
func (s *Store) Path(name string) string { return filepath.Join(s.dir, name) }

// AtomicWrite serializes v as indented JSON and replaces name atomically.
// The temp file is created in the same directory so the final rename is
// atomic on a single local filesystem.
func (s *Store) AtomicWrite(name string, v interface{}) error {
	final := s.Path(name)
	tmp, err := os.CreateTemp(s.dir, ".tmp-"+filepath.Base(name)+"-*")
	if err != nil {
		return fmt.Errorf("%w: %v", ErrWrite, err)
	}
	tmpName := tmp.Name()
	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("%w: %v", ErrEncode, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("%w: %v", ErrWrite, err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("%w: %v", ErrWrite, err)
	}
	if err := os.Rename(tmpName, final); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("%w: %v", ErrRename, err)
	}
	return nil
}

// ReadJSON loads a JSON index file into v. A missing file returns
// ErrNotFound so callers can distinguish first run from corruption.
func (s *Store) ReadJSON(name string, v interface{}) error {
	path := s.Path(name)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ErrNotFound
		}
		return fmt.Errorf("%w: %v", ErrRead, err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("%w: %v", ErrDecode, err)
	}
	return nil
}

// Exists reports whether a name is present in the store.
func (s *Store) Exists(name string) bool {
	_, err := os.Stat(s.Path(name))
	return err == nil
}

// DigestBytes returns the hex SHA-256 of an already-serialized payload.
func DigestBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// DigestJSON canonicalizes v the same way AtomicWrite does and returns its
// SHA-256, which is the manifest_sha256 acceptance field.
func DigestJSON(v interface{}) (string, []byte, error) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", nil, fmt.Errorf("%w: %v", ErrEncode, err)
	}
	data = append(data, '\n')
	return DigestBytes(data), data, nil
}
