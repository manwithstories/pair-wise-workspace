// Package cache stores frame payloads under a content-addressed key and
// refuses any hit whose provenance does not match the caller's frame exactly.
//
// The key is the SHA-256 of the payload, so identical frame content shares one
// entry regardless of which track produced it. Sharing is safe only because a
// hit is still checked against the requesting track, frame number, metadata
// digest and content digest; a mismatch is never a silent hit.
package cache

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"mediad/internal/model"
)

// Magic identifies a cache record file.
const Magic = "MDCACHE1"

// ErrMiss reports that no entry exists for a key. It is not an error condition
// for the pipeline; the frame is simply recomputed.
var ErrMiss = errors.New("cache: miss")

// Provenance is the record stored beside a payload. A hit is valid only if all
// four identity fields match the request.
type Provenance struct {
	TrackID       uint32
	FrameNo       uint64
	MetaDigest    [32]byte
	ContentDigest [32]byte
}

// Key is the content address: the SHA-256 of the payload.
func Key(content [32]byte) string { return fmt.Sprintf("%x", content) }

// Store is a directory-backed cache. It keeps no in-memory index, so its
// footprint is independent of the number of frames.
type Store struct {
	dir string
}

// Open prepares dir, creating it if needed.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, model.Fatal("cache.open", err)
	}
	return &Store{dir: dir}, nil
}

// path returns the file for a key. Keys are hex digests, so they are safe as
// filenames and cannot escape dir.
func (s *Store) path(key string) string { return filepath.Join(s.dir, key+".mdc") }

// ErrInvalid signals a record that fails provenance or integrity checking.
// Callers must treat it exactly like a miss and recompute.
var ErrInvalid = model.ErrCacheInvalid

// Put writes a payload and its provenance atomically. The record encodes the
// payload length and a digest, so a torn write is detected on read rather than
// trusted.
func (s *Store) Put(prov Provenance, payload []byte) error {
	if prov.ContentDigest != model.DigestBytes(payload) {
		return fmt.Errorf("%w: payload does not match its content digest", ErrInvalid)
	}
	tmp, err := os.CreateTemp(s.dir, ".cache-*.tmp")
	if err != nil {
		return model.Fatal("cache.put", err)
	}
	name := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			tmp.Close()
			os.Remove(name)
		}
	}()
	var head [8 + 4 + 8 + 32 + 32 + 4]byte
	copy(head[:], Magic)
	off := 8
	binary.BigEndian.PutUint32(head[off:], prov.TrackID)
	off += 4
	binary.BigEndian.PutUint64(head[off:], prov.FrameNo)
	off += 8
	copy(head[off:], prov.MetaDigest[:])
	off += 32
	copy(head[off:], prov.ContentDigest[:])
	off += 32
	binary.BigEndian.PutUint32(head[off:], uint32(len(payload)))
	off += 4
	if _, err := tmp.Write(head[:off]); err != nil {
		return model.Fatal("cache.put", err)
	}
	if _, err := tmp.Write(payload); err != nil {
		return model.Fatal("cache.put", err)
	}
	if err := tmp.Sync(); err != nil {
		return model.Fatal("cache.put", err)
	}
	if err := tmp.Close(); err != nil {
		return model.Fatal("cache.put", err)
	}
	ok = true
	if err := os.Rename(name, s.path(Key(prov.ContentDigest))); err != nil {
		return model.Fatal("cache.put", err)
	}
	return nil
}

// Get returns the payload only if the stored record matches want on track,
// frame, metadata digest and content digest, and the payload still hashes to
// the recorded content digest. Any mismatch returns ErrInvalid so the caller
// recomputes; ErrMiss means there is simply no record.
func (s *Store) Get(want Provenance) ([]byte, error) {
	f, err := os.Open(s.path(Key(want.ContentDigest)))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrMiss
	}
	if err != nil {
		return nil, model.Fatal("cache.get", err)
	}
	defer f.Close()

	raw, err := io.ReadAll(f)
	if err != nil {
		return nil, ErrInvalid
	}
	const headLen = 8 + 4 + 8 + 32 + 32 + 4
	if len(raw) < headLen || string(raw[:8]) != Magic {
		return nil, fmt.Errorf("%w: malformed record", ErrInvalid)
	}
	off := 8
	got := Provenance{}
	got.TrackID = binary.BigEndian.Uint32(raw[off:])
	off += 4
	got.FrameNo = binary.BigEndian.Uint64(raw[off:])
	off += 8
	copy(got.MetaDigest[:], raw[off:])
	off += 32
	copy(got.ContentDigest[:], raw[off:])
	off += 32
	declared := binary.BigEndian.Uint32(raw[off:])
	off += 4
	payload := raw[off:]
	if uint32(len(payload)) != declared {
		return nil, fmt.Errorf("%w: truncated payload %d/%d", ErrInvalid, len(payload), declared)
	}

	// Provenance: the entry must have been written for this exact request.
	switch {
	case got.TrackID != want.TrackID:
		return nil, fmt.Errorf("%w: track %d != %d", ErrInvalid, got.TrackID, want.TrackID)
	case got.FrameNo != want.FrameNo:
		return nil, fmt.Errorf("%w: frame %d != %d", ErrInvalid, got.FrameNo, want.FrameNo)
	case got.MetaDigest != want.MetaDigest:
		return nil, fmt.Errorf("%w: metadata digest mismatch", ErrInvalid)
	case got.ContentDigest != want.ContentDigest:
		return nil, fmt.Errorf("%w: content digest mismatch", ErrInvalid)
	}
	// Integrity: recompute over the bytes, never trust the stored digest.
	if model.DigestBytes(payload) != want.ContentDigest {
		return nil, fmt.Errorf("%w: payload no longer matches key", ErrInvalid)
	}
	return payload, nil
}

// Verified counts cache outcomes for the report.
type Verified struct {
	Hits       int
	Misses     int
	Invalid    int
	Recomputed int
}

// Record folds one Get/Recompute decision into the counters.
func (v *Verified) Record(err error) {
	switch {
	case err == nil:
		v.Hits++
	case errors.Is(err, ErrMiss):
		v.Misses++
		v.Recomputed++
	default:
		v.Invalid++
		v.Recomputed++
	}
}
