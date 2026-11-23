// Package cache implements the content-addressed stage cache and the
// atomic checkpoints used for interruption recovery. A cache key is
// derived from the stage name, processing parameters, source
// identifiers and the result digests of dependency stages. A hit is
// only honoured after the sources are re-read and their digests
// verified; any mismatch refuses the reuse and reports why.
package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"mediatimeline/internal/mediaerr"
)

// KeyInput is everything a cache key is derived from.
type KeyInput struct {
	Stage   string
	Params  string   // canonical processing parameters
	Sources []string // source identifiers (paths), sorted on use
	Deps    []string // result digests of dependency stages, sorted on use
}

// Key computes the content-addressed key for a stage invocation.
func Key(in KeyInput) string {
	src := append([]string(nil), in.Sources...)
	sort.Strings(src)
	dep := append([]string(nil), in.Deps...)
	sort.Strings(dep)
	h := sha256.New()
	fmt.Fprintf(h, "stage=%s\nparams=%s\n", in.Stage, in.Params)
	for _, s := range src {
		fmt.Fprintf(h, "src=%s\n", s)
	}
	for _, d := range dep {
		fmt.Fprintf(h, "dep=%s\n", d)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Entry is one cached stage result. SourceDigests records the digests
// the sources had when the entry was written; a hit is validated by
// re-reading the sources and comparing.
type Entry struct {
	Key           string            `json:"key"`
	Stage         string            `json:"stage"`
	SourceDigests map[string]string `json:"source_digests"`
	ResultDigest  string            `json:"result_digest"`
	Payload       json.RawMessage   `json:"payload"`
}

// Store is a directory of cache entries named by key.
type Store struct {
	Dir string
}

func (s *Store) path(key string) string { return filepath.Join(s.Dir, key+".json") }

// Put writes an entry atomically (temp file + rename).
func (s *Store) Put(e *Entry) error {
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path(e.Key) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path(e.Key))
}

// Get reads and structurally validates an entry. A tampered entry
// (unparseable, or whose embedded key does not match the requested
// key) is rejected with a recoverable media error.
func (s *Store) Get(key string) (*Entry, error) {
	data, err := os.ReadFile(s.path(key))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil // clean miss
	}
	if err != nil {
		return nil, err
	}
	var e Entry
	if err := json.Unmarshal(data, &e); err != nil {
		return nil, mediaerr.New(mediaerr.Cache, "", mediaerr.NA, mediaerr.NA, true,
			"cache entry %s unreadable (tampered?): %v", key[:12], err)
	}
	if e.Key != key {
		return nil, mediaerr.New(mediaerr.Cache, "", mediaerr.NA, mediaerr.NA, true,
			"cache entry key mismatch: file=%s embedded=%s", key[:12], e.Key[:12])
	}
	return &e, nil
}

// HashFile returns the hex SHA-256 of a file's contents.
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

// ValidateSources re-reads every source recorded in the entry and
// verifies its digest. The first mismatch refuses the reuse with a
// recoverable error naming the source.
func (s *Store) ValidateSources(e *Entry) error {
	ids := make([]string, 0, len(e.SourceDigests))
	for id := range e.SourceDigests {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		got, err := HashFile(id)
		if err != nil {
			return mediaerr.New(mediaerr.Cache, "", mediaerr.NA, mediaerr.NA, true,
				"cache source %s unreadable: %v", id, err)
		}
		if got != e.SourceDigests[id] {
			return mediaerr.New(mediaerr.Cache, "", mediaerr.NA, mediaerr.NA, true,
				"cache source %s digest mismatch: recorded=%s actual=%s",
				id, e.SourceDigests[id][:12], got[:12])
		}
	}
	return nil
}

// DigestSources hashes the given source files into a digest map.
func DigestSources(paths []string) (map[string]string, error) {
	out := make(map[string]string, len(paths))
	for _, p := range paths {
		d, err := HashFile(p)
		if err != nil {
			return nil, err
		}
		out[p] = d
	}
	return out, nil
}

// ResultDigest computes the digest of a stage result payload.
func ResultDigest(payload []byte) string {
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

// Summary joins digests into one canonical string (used in keys).
func Summary(digests ...string) string { return strings.Join(digests, ",") }
