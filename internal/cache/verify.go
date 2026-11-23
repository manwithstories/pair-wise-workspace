package cache

import (
	"encoding/json"
	"os"
	"path/filepath"

	"archive.local/archive/internal/model"
)

// metaVersion guards against reading incompatible cache metadata.
const metaVersion = 1

type metaFile struct {
	Version int    `json:"version"`
	Address string `json:"address"`
	Length  int64  `json:"length"`
	Digest  string `json:"digest"`
	Source  struct {
		ManifestDigest string `json:"manifest_digest"`
		TrackID        uint32 `json:"track_id"`
		Origin         int64  `json:"origin"`
		ByteRange      string `json:"byte_range"`
	} `json:"source"`
}

func (s *Store) saveMeta(e Entry) error {
	m := metaFile{Version: metaVersion, Address: e.Address, Length: e.Length, Digest: e.Digest}
	m.Source.ManifestDigest = e.Source.ManifestDigest
	m.Source.TrackID = e.Source.TrackID
	m.Source.Origin = e.Source.Origin
	m.Source.ByteRange = e.Source.ByteRange
	path := s.metaPath(e.Address)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	// Write to a private temporary file and rename, so a crash never leaves a
	// half-written metadata record.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (s *Store) loadMeta(addr string) (Entry, error) {
	data, err := os.ReadFile(s.metaPath(addr))
	if err != nil {
		return Entry{}, err
	}
	var m metaFile
	if err := json.Unmarshal(data, &m); err != nil {
		return Entry{}, &Mismatch{Address: addr, Reason: "unreadable metadata", Want: "json", Got: err.Error()}
	}
	if m.Version != metaVersion {
		return Entry{}, &Mismatch{Address: addr, Reason: "metadata version",
			Want: "1", Got: string(rune('0' + m.Version))}
	}
	e := Entry{Address: m.Address, Length: m.Length, Digest: m.Digest, Shard: addr[:2]}
	e.Source = Source{ManifestDigest: m.Source.ManifestDigest, TrackID: m.Source.TrackID,
		Origin: m.Source.Origin, ByteRange: m.Source.ByteRange}
	return e, nil
}

// VerifyObject is the standalone integrity check used when a caller already
// holds the expected metadata.
func VerifyObject(data []byte, up Entry, expect Source) error {
	if up.Source.String() != expect.String() {
		return &model.Error{Code: model.CodeCacheMismatch, Sample: -1, OffsetBytes: -1,
			Detail: "cache provenance differs from the current input"}
	}
	if int64(len(data)) != up.Length {
		return &model.Error{Code: model.CodeCacheMismatch, Sample: -1, OffsetBytes: -1,
			Detail: "cache length differs from the recorded length"}
	}
	if Key(data) != up.Digest {
		return &model.Error{Code: model.CodeCacheMismatch, Sample: -1, OffsetBytes: -1,
			Detail: "cache digest differs from the recorded digest"}
	}
	return nil
}

// Provenance builds a source descriptor for a track range.
func Provenance(manifestDigest string, trackID uint32, origin int64, start, end uint64) Source {
	return Source{ManifestDigest: manifestDigest, TrackID: trackID, Origin: origin,
		ByteRange: rangeString(start, end)}
}

func rangeString(start, end uint64) string {
	return itoa(start) + "-" + itoa(end)
}

func itoa(v uint64) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}

// Poison writes corrupted bytes over a cached object, preserving its length
// and metadata. It models a corrupted cache store, so a later Get will detect
// the digest mismatch and quarantine the entry. It returns false when no
// object exists at that address.
func (s *Store) Poison(addr string) (bool, error) {
	path := s.objectPath(addr)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if len(data) == 0 {
		return false, nil
	}
	poisoned := append([]byte(nil), data...)
	poisoned[0] ^= 0xFF
	if err := os.WriteFile(path, poisoned, 0o600); err != nil {
		return false, err
	}
	return true, nil
}

// Contains reports whether an object is present at addr.
func (s *Store) Contains(addr string) bool {
	_, err := os.Stat(s.objectPath(addr))
	return err == nil
}

// Rescan walks every stored metadata record and re-verifies the object it
// describes. Entries that no longer match are quarantined, and the count of
// quarantines attributable to this call is tracked separately from the
// cumulative total.
func (s *Store) Rescan() error {
	prev := s.Quarantined
	root := filepath.Join(s.Root, "meta")
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, shard := range entries {
		if shard.IsDir() == false {
			continue
		}
		files, err := os.ReadDir(filepath.Join(root, shard.Name()))
		if err != nil {
			return err
		}
		for _, f := range files {
			name := f.Name()
			if len(name) < len(".json") || name[len(name)-len(".json"):] != ".json" {
				continue
			}
			addr := name[:len(name)-len(".json")]
			e, err := s.loadMeta(addr)
			if err != nil {
				continue
			}
			data, err := os.ReadFile(s.objectPath(addr))
			if err != nil {
				continue
			}
			if int64(len(data)) != e.Length || Key(data) != e.Digest {
				s.reject(e, "digest mismatch")
			}
		}
	}
	s.lastRescanRejects = s.Quarantined - prev
	return nil
}

// RejectedNow returns quarantines observed during this process, which covers
// both a Rescan and a subsequent Get on a poisoned entry.
func (s *Store) RejectedNow() int {
	return s.Quarantined
}
