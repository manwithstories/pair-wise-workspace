package txn

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"

	"mediarun/internal/model"
	"mediarun/internal/verify"
)

// Recovery describes the outcome of replaying an interrupted run.
type Recovery struct {
	Replayed     int      `json:"replayed"`
	RolledBack   int      `json:"rolled_back"`
	Committed    int      `json:"committed"`
	Truncated    bool     `json:"truncated"`
	Segments     []string `json:"segments"`
	HashMatch    int      `json:"hash_match"`
	HashMismatch int      `json:"hash_mismatch"`
}

// Recover replays the WAL and reconciles it with the manifest on disk.
//
// A segment is considered committed only when the log holds a commit record
// whose hash matches the file on disk and the manifest. Staged segments
// without a commit record are rolled back. Replaying the same log twice
// yields identical results because the manifest is rewritten with the same
// ordered content and all other writes are idempotent.
func Recover(dir string) (*Recovery, error) {
	records, truncated, err := Replay(dir)
	if err != nil {
		return nil, err
	}
	rec := &Recovery{Truncated: truncated}

	current, err := LoadManifest(dir)
	if err != nil {
		return nil, err
	}
	byName := map[string]SegmentCommit{}
	for _, s := range current.Segments {
		byName[s.Name] = s
	}

	// staged holds segments that have a stage record but no commit yet.
	staged := map[string]bool{}
	// fromLog holds the segments whose commit record survived the crash,
	// including any the manifest never got to record.
	fromLog := map[string]SegmentCommit{}
	var logOrder []string
	for _, r := range records {
		rec.Replayed++
		switch r.Op {
		case OpStage:
			if r.Segment != "" {
				staged[r.Segment] = true
			}
		case OpCommit:
			if r.Segment == "" {
				return nil, fmt.Errorf("%w: commit without segment", model.ErrCorruptIndex)
			}
			delete(staged, r.Segment)
			_, hash, err := ReadSegment(dir, r.Segment)
			if err != nil {
				// The payload never reached disk; the commit is void and
				// the run resumes from the previous boundary.
				rec.RolledBack++
				continue
			}
			if hash != r.Hash {
				return nil, fmt.Errorf("%w: segment %q on disk is %s, log says %s", model.ErrCommit, r.Segment, hash, r.Hash)
			}
			if prev, ok := byName[r.Segment]; ok && prev.SHA256 != hash {
				return nil, fmt.Errorf("%w: committed segment %q hash changed", model.ErrCommit, r.Segment)
			}
			commit, ok := fromLog[r.Segment]
			if !ok {
				// The manifest write never happened: rebuild the entry
				// from the segment payload the log committed, so replay
				// converges on the same state as a clean run.
				payload, derr := decodeSegment(dir, r.Segment)
				if derr != nil {
					return nil, derr
				}
				commit = SegmentCommit{
					Name:   payload.Name,
					Start:  int64(payload.Start),
					End:    int64(payload.End),
					Events: len(payload.Events),
					Gaps:   gapPackets(payload.Gaps),
					SHA256: hash,
				}
				fromLog[r.Segment] = commit
				logOrder = append(logOrder, r.Segment)
			}
			rec.Committed++
			rec.Segments = append(rec.Segments, r.Segment)
		case OpRollback:
			delete(staged, r.Segment)
			rec.RolledBack++
		case OpBegin, OpCheck:
			// informational
		default:
			return nil, fmt.Errorf("%w: unknown op %q", model.ErrCorruptIndex, r.Op)
		}
	}
	if len(staged) > 0 {
		rec.RolledBack += len(staged)
	}

	next := &Manifest{
		Version: CurrentManifestVersion,
		Stages:  copyStages(current.Stages),
	}
	// Start from the manifest, but verify every entry against disk so a
	// tampered file cannot be presented as committed.
	for _, s := range current.Segments {
		_, hash, err := ReadSegment(dir, s.Name)
		if err != nil {
			rec.HashMismatch++
			continue
		}
		if hash != s.SHA256 {
			rec.HashMismatch++
			continue
		}
		rec.HashMatch++
		next.Segments = append(next.Segments, s)
	}
	// Adopt segments the log committed but the manifest missed.
	present := map[string]bool{}
	for _, s := range next.Segments {
		present[s.Name] = true
	}
	for _, name := range logOrder {
		if present[name] {
			continue
		}
		next.Segments = append(next.Segments, fromLog[name])
	}
	sort.SliceStable(next.Segments, func(i, j int) bool { return next.Segments[i].Start < next.Segments[j].Start })
	if err := next.Validate(); err != nil {
		return nil, err
	}
	if err := SaveManifest(dir, next); err != nil {
		return nil, err
	}
	return rec, nil
}

// decodeSegment reads a segment payload from disk.
func decodeSegment(dir, name string) (*SegmentPayload, error) {
	data, _, err := ReadSegment(dir, name)
	if err != nil {
		return nil, fmt.Errorf("%w: segment %q unreadable: %v", model.ErrCommit, name, err)
	}
	var p SegmentPayload
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("%w: segment %q payload: %v", model.ErrCorruptIndex, name, err)
	}
	if p.Name != name {
		return nil, fmt.Errorf("%w: segment %q payload names %q", model.ErrCorruptIndex, name, p.Name)
	}
	if p.Version != 1 {
		return nil, fmt.Errorf("%w: segment %q payload version %d", model.ErrCorruptIndex, name, p.Version)
	}
	return &p, nil
}

func gapPackets(gaps []model.Gap) int {
	total := 0
	for _, g := range gaps {
		total += g.Count
	}
	return total
}

// CompareHashes reports whether the two manifests name the same segments
// with the same hashes. This is the "continuous run vs recovered run"
// invariant.
func CompareHashes(a, b *Manifest) bool {
	if len(a.Segments) != len(b.Segments) {
		return false
	}
	am := map[string]string{}
	for _, s := range a.Segments {
		am[s.Name] = s.SHA256
	}
	for _, s := range b.Segments {
		if am[s.Name] != s.SHA256 {
			return false
		}
	}
	return true
}

// VerifyManifest re-reads every committed segment and returns a report.
func VerifyManifest(dir string, m *Manifest) (verify.Report, error) {
	expected := make([]verify.SegmentExpected, 0, len(m.Segments))
	for _, s := range m.Segments {
		expected = append(expected, verify.SegmentExpected{Name: s.Name, Hash: s.SHA256})
	}
	return verify.Segments(dir, expected)
}

// ManifestFile exposes the manifest location for tooling.
func ManifestFile(dir string) string { return filepath.Join(dir, "manifest.json") }
