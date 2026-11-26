// Package index builds GOP/keyframe indexes and persists them atomically.
package index

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"medcheck/internal/media"
)

const IndexVersion = 1

// Segment is one GOP/editing segment with a stable content hash.
type Segment struct {
	Index       int    `json:"index"`
	GOPID       uint32 `json:"gop_id"`
	StartAU     uint32 `json:"start_au"`
	EndAU       uint32 `json:"end_au"` // exclusive
	KeyframeAU  uint32 `json:"keyframe_au"`
	StartMS     int64  `json:"start_ms"`
	EndMS       int64  `json:"end_ms"`
	Hash        string `json:"segment_sha256"`
	KeyframeHit bool   `json:"keyframe_hit"`
}

// Index is the validated GOP/segment manifest.
type Index struct {
	Version           int       `json:"version"`
	Seed              int64     `json:"seed"`
	AUCount           int       `json:"au_count"`
	KeyframesFound    int       `json:"keyframes_found"`
	KeyframesExpected int       `json:"keyframes_expected"`
	Segments          []Segment `json:"segments"`
}

// BuildGOP groups access units by GOPID and verifies each GOP has a keyframe.
func BuildGOP(aus []media.AccessUnit, hashes map[int]string, seed int64) (*Index, []error) {
	idx := &Index{Version: IndexVersion, Seed: seed, AUCount: len(aus)}
	if len(aus) == 0 {
		return idx, []error{errors.New("index: no access units")}
	}
	var errs []error
	byGOP := map[uint32]*Segment{}
	var order []uint32
	for _, au := range aus {
		seg, ok := byGOP[au.GOPID]
		if !ok {
			s := &Segment{Index: len(order), GOPID: au.GOPID, StartAU: au.Index, KeyframeAU: au.Index,
				StartMS: au.PTSMS, Hash: hashes[int(au.GOPID)]}
			if s.Hash == "" {
				s.Hash = HashAUGroup(nil)
			}
			byGOP[au.GOPID] = s
			seg = s
			order = append(order, au.GOPID)
		}
		if au.KeyFrame {
			seg.KeyframeHit = true
			seg.KeyframeAU = au.Index
			idx.KeyframesFound++
		}
		seg.EndAU = au.Index + 1
		seg.EndMS = au.PTSMS
	}
	for _, id := range order {
		seg := byGOP[id]
		idx.Segments = append(idx.Segments, *seg)
		if seg.KeyframeHit {
			idx.KeyframesExpected++
		} else {
			errs = append(errs, fmt.Errorf("index: GOP %d has no keyframe", id))
		}
	}
	// Every generated GOP is expected to provide one hit target.
	idx.KeyframesExpected = len(idx.Segments)
	return idx, errs
}

// HashAUGroup hashes canonical fixed headers. It never hashes payload bytes,
// keeping analysis header-only and deterministic.
func HashAUGroup(aus []media.AccessUnit) string {
	h := sha256.New()
	for _, au := range aus {
		h.Write(media.EncodeHeader(au))
		fmt.Fprintf(h, "offset=%d\n", au.Offset)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Validate checks schema and segment hash shape.
func (idx *Index) Validate() error {
	if idx == nil {
		return errors.New("index: nil index")
	}
	if idx.Version != IndexVersion {
		return fmt.Errorf("index: unsupported version %d", idx.Version)
	}
	if len(idx.Segments) == 0 {
		return errors.New("index: no segments")
	}
	for i, seg := range idx.Segments {
		if seg.Index != i {
			return fmt.Errorf("index: segment order mismatch at %d", i)
		}
		if len(seg.Hash) != 64 {
			return fmt.Errorf("index: segment %d hash is not sha256 hex", i)
		}
	}
	return nil
}

// SaveAtomic writes JSON to a temp file and renames it over path.
func SaveAtomic(path string, idx *Index) error {
	if err := idx.Validate(); err != nil {
		return err
	}
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create index directory: %w", err)
		}
	}
	data, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open temp index: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("write temp index: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("sync temp index: %w", err)
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("atomic rename index: %w", err)
	}
	return nil
}

// Load reads and validates an index.
func Load(path string) (*Index, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var idx Index
	if err := json.Unmarshal(data, &idx); err != nil {
		return nil, fmt.Errorf("parse index: %w", err)
	}
	if err := idx.Validate(); err != nil {
		return nil, err
	}
	return &idx, nil
}

// ResumeOrBuild loads a valid checkpoint for expectedSeed; otherwise builds
// and atomically saves a fresh index.
func ResumeOrBuild(path string, expectedSeed int64, build func() (*Index, error)) (idx *Index, resumed bool, err error) {
	if existing, loadErr := Load(path); loadErr == nil {
		if existing.Seed == expectedSeed {
			return existing, true, nil
		}
		_ = os.Remove(path)
	} else if !errors.Is(loadErr, os.ErrNotExist) {
		// A corrupt index is rebuilt rather than resuming from unsafe state.
		_ = os.Remove(path)
	}
	idx, err = build()
	if err != nil {
		return nil, false, err
	}
	if err := SaveAtomic(path, idx); err != nil {
		return nil, false, err
	}
	return idx, false, nil
}
