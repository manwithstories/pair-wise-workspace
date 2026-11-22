// Package verify is the fan-in stage: it recomputes segment hashes and
// compares segment start/end times across representations. A boundary
// deviation larger than one video frame fails the run and names the
// representation, the segment index and the delta.
package verify

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"

	"mediac/internal/model"
	"mediac/internal/segment"
)

// SegmentPath is the canonical on-disk location of a segment file.
func SegmentPath(dir, repID string, index int) string {
	return filepath.Join(dir, "segments", repID, fmt.Sprintf("%06d.seg", index))
}

// Hashes recomputes the SHA-256 of every segment file in the manifest.
func Hashes(dir string, m *segment.Manifest) *model.MediaError {
	for _, rep := range m.Representations {
		for _, seg := range rep.Segments {
			b, err := os.ReadFile(SegmentPath(dir, rep.ID, seg.Index))
			if err != nil {
				me := model.New(model.StageVerify, "segment_missing", err.Error(), 1)
				me.Representation = rep.ID
				me.SegmentIndex = seg.Index
				return me
			}
			sum := sha256.Sum256(b)
			if got := fmt.Sprintf("%x", sum); got != seg.SHA256 {
				me := model.New(model.StageVerify, "hash_mismatch",
					fmt.Sprintf("segment %s#%d sha256 %s != manifest %s",
						rep.ID, seg.Index, got, seg.SHA256), 1)
				me.Representation = rep.ID
				me.SegmentIndex = seg.Index
				return me
			}
		}
	}
	return nil
}

// FanIn compares segment boundaries across representations and returns
// the maximum observed delta in milliseconds. A delta larger than
// frameMs fails with exit code 3.
func FanIn(m *segment.Manifest, frameMs int64) (int64, *model.MediaError) {
	var maxDelta int64
	if len(m.Representations) == 0 {
		return 0, nil
	}
	n := len(m.Representations[0].Segments)
	for i := 0; i < n; i++ {
		var loS, hiS, loE, hiE int64
		var repS, repE string
		for j, rep := range m.Representations {
			if i >= len(rep.Segments) {
				me := model.New(model.StageVerify, "segment_count",
					fmt.Sprintf("%s has %d segments, want %d", rep.ID, len(rep.Segments), n),
					model.ExitBoundaryMismatch)
				me.Representation = rep.ID
				me.SegmentIndex = i
				return maxDelta, me
			}
			s := rep.Segments[i]
			if j == 0 || s.StartMs < loS {
				loS = s.StartMs
			}
			if j == 0 || s.StartMs > hiS {
				hiS, repS = s.StartMs, rep.ID
			}
			if j == 0 || s.EndMs < loE {
				loE = s.EndMs
			}
			if j == 0 || s.EndMs > hiE {
				hiE, repE = s.EndMs, rep.ID
			}
		}
		dS, dE := hiS-loS, hiE-loE
		if dS > maxDelta {
			maxDelta = dS
		}
		if dE > maxDelta {
			maxDelta = dE
		}
		if dS > frameMs || dE > frameMs {
			d, rep := dS, repS
			if dE > dS {
				d, rep = dE, repE
			}
			me := model.New(model.StageVerify, "boundary_drift",
				fmt.Sprintf("segment %d of %s deviates %d ms (>%d ms frame)",
					i, rep, d, frameMs), model.ExitBoundaryMismatch)
			me.Representation = rep
			me.SegmentIndex = i
			me.DeltaMs = d
			return maxDelta, me
		}
	}
	return maxDelta, nil
}
