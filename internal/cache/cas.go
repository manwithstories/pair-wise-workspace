// Package cache implements a content-addressed store for reusable pipeline
// artifacts. A cache key is derived from the input digest, the processing
// configuration and the FFmpeg version, so a changed source, a changed config
// or a changed toolchain all force a rebuild. On a hit the caller still
// re-verifies every output digest; any mismatch invalidates the entry.
package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/fashionweek/mediad/internal/media"
)

// Entry is the on-disk record describing one cached result.
type Entry struct {
	Key       string            `json:"key"`
	InputHash string            `json:"input_hash"`
	ConfigHash string           `json:"config_hash"`
	Tool      string            `json:"tool"`
	Outputs   map[string]string `json:"outputs"` // logical name -> sha256
}

// Store is a directory-backed CAS.
type Store struct {
	root string
}

// Open creates or opens a cache rooted at root.
func Open(root string) (*Store, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, media.New(media.PhaseCache, media.KindTool, root, -1,
			"cannot create cache root").Wrap(err)
	}
	return &Store{root: root}, nil
}

// Root exposes the cache directory.
func (s *Store) Root() string { return s.root }

// HashBytes returns the hex sha256 of data.
func HashBytes(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// HashFile streams a file through sha256 without loading it fully.
func HashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", media.New(media.PhaseCache, media.KindTool, path, -1,
			"cannot open file for hashing").Wrap(err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", media.New(media.PhaseCache, media.KindTool, path, -1,
			"error while hashing file").Wrap(err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Key derives the content key from the input hash, config blob and tool id.
func Key(inputHash string, config []byte, tool string) string {
	cfgHash := HashBytes(config)
	combined := HashBytes([]byte(inputHash + "|" + cfgHash + "|" + tool))
	return combined
}

func (s *Store) entryPath(key string) string {
	return filepath.Join(s.root, key[:2], key+".json")
}

// Put writes an entry and stores its output blobs under the CAS. The caller
// provides the output files by logical name; they are copied and hashed.
func (s *Store) Put(key, inputHash, tool string, config []byte,
	outputs map[string]string) (*Entry, error) {
	entry := &Entry{
		Key: key, InputHash: inputHash,
		ConfigHash: HashBytes(config), Tool: tool,
		Outputs: map[string]string{},
	}
	names := make([]string, 0, len(outputs))
	for name := range outputs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		src := outputs[name]
		hash, err := HashFile(src)
		if err != nil {
			return nil, err
		}
		dst := s.blobPath(hash)
		if err := copyAtomic(src, dst); err != nil {
			return nil, err
		}
		entry.Outputs[name] = hash
	}
	if err := s.writeEntry(entry); err != nil {
		return nil, err
	}
	return entry, nil
}

func (s *Store) blobPath(hash string) string {
	return filepath.Join(s.root, "blobs", hash[:2], hash)
}

// Get loads an entry. A missing entry returns (nil, nil).
func (s *Store) Get(key string) (*Entry, error) {
	data, err := os.ReadFile(s.entryPath(key))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, media.New(media.PhaseCache, media.KindTool, s.entryPath(key), -1,
			"cannot read cache entry").Wrap(err)
	}
	var e Entry
	if err := json.Unmarshal(data, &e); err != nil {
		return nil, media.New(media.PhaseCache, media.KindDigest, s.entryPath(key), -1,
			"corrupt cache entry json").Wrap(err)
	}
	return &e, nil
}

// Verify re-validates a hit: the input must still hash to the recorded value
// and every cached output blob must exist with the recorded digest.
func (s *Store) Verify(e *Entry, currentInputHash string) error {
	if e.InputHash != currentInputHash {
		return media.New(media.PhaseCache, media.KindDigest, "", -1,
			"cache invalidated: input source changed")
	}
	names := make([]string, 0, len(e.Outputs))
	for name := range e.Outputs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		want := e.Outputs[name]
		blob := s.blobPath(want)
		got, err := HashFile(blob)
		if err != nil {
			return media.New(media.PhaseCache, media.KindDigest, blob, -1,
				"cached output missing or unreadable: "+name).Wrap(err)
		}
		if got != want {
			return media.New(media.PhaseCache, media.KindDigest, blob, -1,
				"cached output digest mismatch: "+name)
		}
	}
	return nil
}

// Restore copies a verified output blob back into destPath.
func (s *Store) Restore(e *Entry, name, destPath string) error {
	want, ok := e.Outputs[name]
	if !ok {
		return media.New(media.PhaseCache, media.KindBounds, "", -1,
			"cache entry has no output named "+name)
	}
	return copyAtomic(s.blobPath(want), destPath)
}

func (s *Store) writeEntry(e *Entry) error {
	path := s.entryPath(e.Key)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return media.New(media.PhaseCache, media.KindTool, path, -1,
			"cannot create entry dir").Wrap(err)
	}
	data, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return media.New(media.PhaseCache, media.KindTool, path, -1,
			"cannot marshal cache entry").Wrap(err)
	}
	return writeFileAtomic(path, data)
}
