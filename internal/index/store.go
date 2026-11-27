package index

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// ErrStoreCorrupt marks a cached index whose embedded digest does not
// match the key it was stored under.
var ErrStoreCorrupt = errors.New("corrupt index store entry")

// indexDir is the on-disk directory name holding content-addressed indexes
// and pipeline metadata inside an output directory.
const indexDir = ".mediad"

// Store persists FileIndex records keyed by the source SHA-256. A changed
// source file produces a different digest and therefore a fresh build.
type Store struct {
	root string
}

// NewStore opens (lazily creating) the index store beneath outDir.
func NewStore(outDir string) (*Store, error) {
	root := filepath.Join(outDir, indexDir, "index")
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	return &Store{root: root}, nil
}

// Root returns the absolute root directory of the store.
func (s *Store) Root() string { return s.root }

func (s *Store) path(digest string) string {
	return filepath.Join(s.root, digest[:2], digest+".json")
}

// Load returns the cached index for digest. A miss (not an error) yields
// (nil, nil) so callers can decide to rebuild.
func (s *Store) Load(digest string) (*FileIndex, error) {
	b, err := os.ReadFile(s.path(digest))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var idx FileIndex
	if err := json.Unmarshal(b, &idx); err != nil {
		return nil, err
	}
	if idx.SHA256 != digest {
		return nil, fmt.Errorf("%w: cached index digest %s != requested %s",
			ErrStoreCorrupt, idx.SHA256, digest)
	}
	return &idx, nil
}

// Save atomically writes an index, fsyncing before rename.
func (s *Store) Save(idx *FileIndex) error {
	if idx.SHA256 == "" {
		return fmt.Errorf("refusing to store index without digest")
	}
	b, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return err
	}
	target := s.path(idx.SHA256)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	tmp := target + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
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
	return os.Rename(tmp, target)
}

// GetOrScan returns the cached index for the file at path or scans it and
// caches the result. The returned ScanResult reflects the current content.
func (s *Store) GetOrScan(path string) (*ScanResult, error) {
	digest, err := HashFile(path)
	if err != nil {
		return nil, err
	}
	if cached, err := s.Load(digest); err == nil && cached != nil {
		return &ScanResult{Index: cached, Err: nil}, nil
	}
	res, err := Scan(path)
	if err != nil && res == nil {
		return nil, err
	}
	if saveErr := s.Save(res.Index); saveErr != nil {
		return nil, saveErr
	}
	return res, nil
}

// HashFile streams the SHA-256 digest of a file and hex-encodes it.
func HashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
