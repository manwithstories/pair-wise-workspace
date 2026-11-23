package index

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"os"
	"path/filepath"
	"sort"

	"archive/internal/media"
)

// Store commits index segments to a local directory. Every segment is
// written to a temp file, validated by re-reading and comparing its
// digest, then atomically renamed into place. Re-committing an identical
// segment is a no-op, so reruns never duplicate segments.
type Store struct {
	dir string
}

// Open creates (if needed) and returns the store rooted at dir.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, media.New(media.KindInternal, "index.store", "", -1, "create index dir", err)
	}
	return &Store{dir: dir}, nil
}

// EncodeEntries serializes entries deterministically (sorted by track,
// then decode time) so identical input always yields identical bytes.
func EncodeEntries(es []Entry) []byte {
	sorted := append([]Entry(nil), es...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Track != sorted[j].Track {
			return sorted[i].Track < sorted[j].Track
		}
		return sorted[i].DTSus < sorted[j].DTSus
	})
	var buf bytes.Buffer
	binary.Write(&buf, binary.LittleEndian, uint32(len(sorted)))
	for _, e := range sorted {
		tr := []byte(e.Track)
		binary.Write(&buf, binary.LittleEndian, uint16(len(tr)))
		buf.Write(tr)
		binary.Write(&buf, binary.LittleEndian, uint32(e.Seq))
		binary.Write(&buf, binary.LittleEndian, e.DTSus)
		binary.Write(&buf, binary.LittleEndian, e.PTSus)
		binary.Write(&buf, binary.LittleEndian, e.ByteOffset)
		binary.Write(&buf, binary.LittleEndian, e.Size)
		binary.Write(&buf, binary.LittleEndian, e.PrevDTSus)
	}
	return buf.Bytes()
}

// CommitSegment atomically commits one segment. It returns false when the
// segment already exists with identical content (idempotent rerun).
func (s *Store) CommitSegment(key string, data []byte) (bool, error) {
	final := filepath.Join(s.dir, "seg-"+key+".idx")
	want := sha256.Sum256(data)
	if existing, err := os.ReadFile(final); err == nil {
		if sha256.Sum256(existing) == want {
			return false, nil // already committed, do not duplicate
		}
		return false, media.Corruptf("index.store", "", -1,
			"conflicting segment content for key "+key)
	}
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return false, media.New(media.KindInternal, "index.store", "", -1, "write temp segment", err)
	}
	// Validate: re-read the temp file and compare digests before rename.
	back, err := os.ReadFile(tmp)
	if err != nil {
		os.Remove(tmp)
		return false, media.New(media.KindInternal, "index.store", "", -1, "re-read temp segment", err)
	}
	if sha256.Sum256(back) != want || len(back) != len(data) {
		os.Remove(tmp)
		return false, media.Corruptf("index.store", "", -1,
			"temp segment validation failed (digest mismatch)")
	}
	if err := os.Rename(tmp, final); err != nil {
		os.Remove(tmp)
		return false, media.New(media.KindInternal, "index.store", "", -1, "atomic rename", err)
	}
	return true, nil
}

// Finalize writes the index manifest (sorted segment keys + digests) and
// returns the overall index digest. Identical segments always produce the
// same digest, which is the rerun-stability guarantee.
func (s *Store) Finalize() (string, error) {
	segs, err := filepath.Glob(filepath.Join(s.dir, "seg-*.idx"))
	if err != nil {
		return "", media.New(media.KindInternal, "index.store", "", -1, "list segments", err)
	}
	sort.Strings(segs)
	var manifest bytes.Buffer
	for _, p := range segs {
		data, err := os.ReadFile(p)
		if err != nil {
			return "", media.New(media.KindInternal, "index.store", "", -1, "read segment", err)
		}
		sum := sha256.Sum256(data)
		manifest.WriteString(filepath.Base(p))
		manifest.WriteByte(' ')
		manifest.WriteString(hex.EncodeToString(sum[:]))
		manifest.WriteByte('\n')
	}
	if err := os.WriteFile(filepath.Join(s.dir, "manifest.txt"), manifest.Bytes(), 0o644); err != nil {
		return "", media.New(media.KindInternal, "index.store", "", -1, "write manifest", err)
	}
	total := sha256.Sum256(manifest.Bytes())
	return hex.EncodeToString(total[:]), nil
}
