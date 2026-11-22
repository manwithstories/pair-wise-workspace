// Package cache implements a content-addressed store binding each
// output's digest to its normalized provenance. A cache hit is only
// trusted after both the stored bytes and the provenance re-hash to
// the recorded digest; any mismatch quarantines the entry.
package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"archive/internal/fault"
)

// Object is the manifest entry recorded next to each blob.
type Object struct {
	Digest string `json:"digest"` // hex sha256 of Blob bytes
	Source string `json:"source"` // normalized provenance (manifest URI)
	Bytes  int64  `json:"bytes"`
}

// Store is a CAS rooted at a directory.
type Store struct {
	root string
}

// Open creates the store layout under root.
func Open(root string) (*Store, error) {
	s := &Store{root: root}
	for _, d := range []string{s.blobsDir(), s.metaDir(), s.tmpDir(), s.quarantineDir()} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, fault.Wrap(fault.Fatal, "cache", root, err)
		}
	}
	return s, nil
}

func (s *Store) blobsDir() string      { return filepath.Join(s.root, "blobs") }
func (s *Store) metaDir() string       { return filepath.Join(s.root, "meta") }
func (s *Store) tmpDir() string        { return filepath.Join(s.root, "tmp") }
func (s *Store) quarantineDir() string { return filepath.Join(s.root, "quarantine") }

// Digest returns the hex sha256 of data.
func Digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// ValidateSource mirrors the manifest provenance rules: a source must
// be non-empty, slash-separated, relative and free of parent escapes.
func ValidateSource(src string) error {
	if src == "" {
		return fault.Newf(fault.Fatal, "cache", "", "empty provenance")
	}
	if strings.ContainsAny(src, "\\\x00") {
		return fault.Newf(fault.Fatal, "cache", src, "invalid provenance characters")
	}
	if strings.HasPrefix(src, "/") || strings.Contains(src, "://") {
		return fault.Newf(fault.Fatal, "cache", src, "absolute provenance")
	}
	for _, part := range strings.Split(src, "/") {
		if part == ".." {
			return fault.Newf(fault.Fatal, "cache", src, "provenance escapes base")
		}
	}
	return nil
}

// Put stores data under source. It writes to a temp file then renames,
// so a crash never leaves a half-written blob adjacent to its metadata.
// existing reports a verified hit that was already present.
func (s *Store) Put(source string, data []byte) (obj Object, existing bool, err error) {
	if err := ValidateSource(source); err != nil {
		return Object{}, false, err
	}
	if int64(len(data)) > 64<<20 {
		return Object{}, false, fault.Newf(fault.Fatal, "cache", source, "segment exceeds 64 MiB")
	}
	obj = Object{Digest: Digest(data), Source: source, Bytes: int64(len(data))}
	if _, statErr := os.Stat(s.blobPath(obj.Digest)); statErr == nil {
		if _, verr := s.verify(obj); verr == nil {
			return obj, true, nil
		}
		// Mismatch: the old blob is untrustworthy. Quarantine it and
		// recompute rather than serving a corrupt hit.
		if err := s.quarantine(obj.Digest); err != nil {
			return Object{}, false, err
		}
	}
	if err := s.writeAtomic(obj, data); err != nil {
		return Object{}, false, err
	}
	return obj, false, nil
}

// Get re-verifies a cached object against both its digest and its
// recorded provenance before returning the bytes.
func (s *Store) Get(digest, source string) ([]byte, error) {
	obj := Object{Digest: digest, Source: source}
	meta, err := s.readMeta(digest)
	if err != nil {
		return nil, err
	}
	if meta.Source != source {
		return nil, fault.Newf(fault.Fatal, "cache", digest,
			"provenance mismatch: stored %q requested %q", meta.Source, source)
	}
	data, err := os.ReadFile(s.blobPath(digest))
	if err != nil {
		return nil, fault.Wrap(fault.Fatal, "cache", digest, err)
	}
	if got := Digest(data); got != digest {
		return nil, fault.Newf(fault.Fatal, "cache", digest, "blob digest mismatch on read")
	}
	obj.Bytes = int64(len(data))
	return data, nil
}

// Verified reports whether digest is present and matches source.
func (s *Store) Verified(digest, source string) bool {
	_, err := s.verify(Object{Digest: digest, Source: source})
	return err == nil
}

func (s *Store) verify(obj Object) (Object, error) {
	meta, err := s.readMeta(obj.Digest)
	if err != nil {
		return Object{}, err
	}
	if meta.Source != obj.Source {
		return Object{}, fault.Newf(fault.Fatal, "cache", obj.Digest, "provenance mismatch")
	}
	data, err := os.ReadFile(s.blobPath(obj.Digest))
	if err != nil {
		return Object{}, fault.Wrap(fault.Fatal, "cache", obj.Digest, err)
	}
	if Digest(data) != obj.Digest || int64(len(data)) != meta.Bytes {
		return Object{}, fault.Newf(fault.Fatal, "cache", obj.Digest, "digest mismatch")
	}
	return meta, nil
}

func (s *Store) blobPath(digest string) string { return filepath.Join(s.blobsDir(), digest) }
func (s *Store) metaPath(digest string) string { return filepath.Join(s.metaDir(), digest+".json") }

func (s *Store) readMeta(digest string) (Object, error) {
	if err := checkDigest(digest); err != nil {
		return Object{}, err
	}
	raw, err := os.ReadFile(s.metaPath(digest))
	if err != nil {
		return Object{}, fault.Wrap(fault.Fatal, "cache", digest, err)
	}
	var obj Object
	if err := json.Unmarshal(raw, &obj); err != nil {
		return Object{}, fault.Wrap(fault.Fatal, "cache", digest, err)
	}
	return obj, nil
}

func (s *Store) writeAtomic(obj Object, data []byte) error {
	tmp, err := os.CreateTemp(s.tmpDir(), "put-*")
	if err != nil {
		return fault.Wrap(fault.Fatal, "cache", obj.Source, err)
	}
	name := tmp.Name()
	defer os.Remove(name) // no-op after a successful rename
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fault.Wrap(fault.Fatal, "cache", obj.Source, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fault.Wrap(fault.Fatal, "cache", obj.Source, err)
	}
	if err := tmp.Close(); err != nil {
		return fault.Wrap(fault.Fatal, "cache", obj.Source, err)
	}
	raw, err := json.Marshal(obj)
	if err != nil {
		return fault.Wrap(fault.Fatal, "cache", obj.Source, err)
	}
	if err := os.WriteFile(name+".json", raw, 0o644); err != nil {
		return fault.Wrap(fault.Fatal, "cache", obj.Source, err)
	}
	// Blob first, metadata last: a crash between the two leaves an
	// orphan blob, which a later Put overwrites, never a valid-looking
	// hit with missing provenance.
	if err := os.Rename(name, s.blobPath(obj.Digest)); err != nil {
		return fault.Wrap(fault.Fatal, "cache", obj.Source, err)
	}
	if err := os.Rename(name+".json", s.metaPath(obj.Digest)); err != nil {
		return fault.Wrap(fault.Fatal, "cache", obj.Source, err)
	}
	return nil
}

func (s *Store) quarantine(digest string) error {
	// Name each artifact uniquely: a retry in the same process must not
	// clobber the evidence from the first detection.
	for _, from := range []string{s.blobPath(digest), s.metaPath(digest)} {
		to, err := uniqueName(filepath.Join(s.quarantineDir(), filepath.Base(from)))
		if err != nil {
			return fault.Wrap(fault.Fatal, "cache", digest, err)
		}
		if err := os.Rename(from, to); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fault.Wrap(fault.Fatal, "cache", digest, err)
		}
	}
	return nil
}

func uniqueName(base string) (string, error) {
	for i := 0; i < 1000; i++ {
		cand := fmt.Sprintf("%s.%d.%d", base, os.Getpid(), i)
		if _, err := os.Stat(cand); errors.Is(err, os.ErrNotExist) {
			return cand, nil
		}
	}
	return "", fmt.Errorf("cache: cannot allocate quarantine name for %s", base)
}

func checkDigest(d string) error {
	if len(d) != 64 {
		return fault.Newf(fault.Fatal, "cache", d, "malformed digest")
	}
	if _, err := hex.DecodeString(d); err != nil {
		return fault.Newf(fault.Fatal, "cache", d, "malformed digest")
	}
	return nil
}
