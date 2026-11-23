// Package cache stores archive objects by content address. A cache hit is
// never trusted on its address alone: provenance, length and full SHA-256 are
// re-verified, and any mismatch quarantines the entry so it is reprocessed.
package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Key is the content address of an object: the SHA-256 of its bytes.
func Key(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Source identifies where an object came from, so a cache hit can prove it is
// the same input and not merely the same bytes.
type Source struct {
	ManifestDigest string // digest of the manifest that declared the object
	TrackID        uint32
	Origin         int64  // first source tick
	ByteRange      string // e.g. "1116-12927"
}

// String renders the provenance in a stable order for comparison and storage.
func (s Source) String() string {
	return fmt.Sprintf("manifest=%s track=%d origin=%d range=%s",
		s.ManifestDigest, s.TrackID, s.Origin, s.ByteRange)
}

// Entry is the metadata stored beside a cached object.
type Entry struct {
	Address string
	Length  int64
	Digest  string
	Source  Source
	Shard   string
}

// Store is a content-addressed cache rooted at an explicit directory.
type Store struct {
	Root string
	// Rejected lists addresses quarantined during this run, for reporting.
	Rejected []string
	// Hits and Misses are counters for diagnostics.
	Hits, Misses, Quarantined int
	lastRescanRejects         int
}

// NewStore creates (or reuses) a cache rooted at root. The directory is
// created with owner-only permissions.
func NewStore(root string) (*Store, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return nil, err
	}
	return &Store{Root: root}, nil
}

// Put writes an object under its content address. It refuses to overwrite an
// existing object with different bytes, quarantining the old one instead.
func (s *Store) Put(data []byte, src Source) (Entry, error) {
	addr := Key(data)
	e := Entry{Address: addr, Length: int64(len(data)), Digest: addr, Source: src,
		Shard: addr[:2]}
	path := s.objectPath(addr)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return Entry{}, err
	}
	if existing, err := os.ReadFile(path); err == nil {
		if Key(existing) == addr {
			// Same bytes already cached; refresh the metadata to the latest
			// provenance only when it matches.
			s.saveMeta(e)
			return e, nil
		}
		// Address collision or prior corruption: quarantine and rewrite.
		s.quarantine(path, addr)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return Entry{}, err
	}
	if err := s.saveMeta(e); err != nil {
		return Entry{}, err
	}
	return e, nil
}

// Verify re-validates an object against its stored entry. It returns the
// bytes only when provenance, length and digest all match.
func (s *Store) Verify(e Entry, expect Source) ([]byte, error) {
	if e.Source.String() != expect.String() {
		s.reject(e, "provenance mismatch")
		return nil, &Mismatch{Address: e.Address, Reason: "provenance mismatch",
			Want: expect.String(), Got: e.Source.String()}
	}
	path := s.objectPath(e.Address)
	data, err := os.ReadFile(path)
	if err != nil {
		s.Misses++
		return nil, err
	}
	if int64(len(data)) != e.Length {
		s.reject(e, "length mismatch")
		return nil, &Mismatch{Address: e.Address, Reason: "length mismatch",
			Want: fmt.Sprintf("%d", e.Length), Got: fmt.Sprintf("%d", len(data))}
	}
	if Key(data) != e.Digest || e.Digest != e.Address {
		s.reject(e, "digest mismatch")
		return nil, &Mismatch{Address: e.Address, Reason: "digest mismatch",
			Want: e.Digest, Got: Key(data)}
	}
	return data, nil
}

// Get looks up an object by address and provenance, verifying it on the way.
func (s *Store) Get(addr string, expect Source) ([]byte, Entry, error) {
	e, err := s.loadMeta(addr)
	if err != nil {
		s.Misses++
		return nil, Entry{}, err
	}
	data, err := s.Verify(e, expect)
	if err != nil {
		return nil, e, err
	}
	s.Hits++
	return data, e, nil
}

// Mismatch is returned when a cached object fails verification.
type Mismatch struct {
	Address string
	Reason  string
	Want    string
	Got     string
}

func (m *Mismatch) Error() string {
	return fmt.Sprintf("cache %s: %s (want %s, got %s)", m.Address, m.Reason, m.Want, m.Got)
}

// reject quarantines both the metadata record and the object bytes, so the
// bad entry can never be served again and a later Put starts clean.
func (s *Store) reject(e Entry, reason string) {
	s.Quarantined++
	s.Rejected = append(s.Rejected, e.Address)
	_ = s.quarantine(s.metaPath(e.Address), e.Address)
	_ = s.quarantine(s.objectPath(e.Address), e.Address)
}

// quarantine moves a bad entry aside under a .bad suffix so it can never be
// served again; the suffix preserves the address for diagnosis. A missing file
// is treated as already quarantined.
func (s *Store) quarantine(path, addr string) error {
	bad := path + ".bad"
	if err := os.Rename(path, bad); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return nil
}

func (s *Store) objectPath(addr string) string {
	return filepath.Join(s.Root, "objects", addr[:2], addr+".bin")
}

func (s *Store) metaPath(addr string) string {
	return filepath.Join(s.Root, "meta", addr[:2], addr+".json")
}

// QuarantinedEntries lists .bad files left by this store, for cleanup checks.
func (s *Store) QuarantinedEntries() ([]string, error) {
	var out []string
	root := filepath.Join(s.Root, "objects")
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		if strings.HasSuffix(d.Name(), ".bad") {
			out = append(out, path)
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

// Digest of a file on disk, used to fingerprint cache inputs.
func FileDigest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	buf := make([]byte, 1<<20)
	for {
		n, err := io.ReadFull(f, buf)
		h.Write(buf[:n])
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break
		}
		if err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
