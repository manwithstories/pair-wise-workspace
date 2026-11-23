// Package verify independently recomputs segment hashes, parses
// round-trip summaries and checks keyframe index hit rates so that
// delivered fragments can be proven unaltered.
package verify

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"

	"github.com/example/mediad/internal/ingest"
	"github.com/example/mediad/internal/model"
)

// SegmentRecord is one entry of the delivery manifest.
type SegmentRecord struct {
	ID            string `json:"id"`
	Path          string `json:"path"`
	SHA256        string `json:"sha256"`
	Size          int64  `json:"size"`
	KeyframeCount int    `json:"keyframe_count"`
	SampleCount   uint64 `json:"sample_count"`
	TrackID       uint32 `json:"track_id"`
	Rejected      bool   `json:"rejected,omitempty"`
	RejectReason  string `json:"reject_reason,omitempty"`
}

// HashFile recomputes the SHA-256 of a file using only the standard
// library, streaming so memory stays bounded.
func HashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, model.Wrap(model.KindChecksum, "verify", "hash",
			path, err)
	}
	defer f.Close()
	sum := sha256.New()
	n, err := io.Copy(sum, f)
	if err != nil {
		return "", 0, model.Wrap(model.KindChecksum, "verify", "hash",
			path, err)
	}
	return hex.EncodeToString(sum.Sum(nil)), n, nil
}

// HashBytes hashes an in-memory payload.
func HashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Manifest is the full delivery verification document.
type Manifest struct {
	Segments []SegmentRecord `json:"segments"`
}

// RecomputeHashes independently re-hashes every manifest segment and
// compares against the recorded digest. It returns the number of
// matches and a checksum error on the first mismatch.
func RecomputeHashes(m *Manifest) (matched int, err error) {
	for _, rec := range m.Segments {
		if rec.Rejected {
			continue
		}
		got, _, err := HashFile(rec.Path)
		if err != nil {
			return matched, err
		}
		if got != rec.SHA256 {
			return matched, model.New(model.KindChecksum, "verify",
				"rehash", rec.ID+": digest mismatch")
		}
		matched++
	}
	return matched, nil
}

// KeyframeHitRate returns the fraction of planned keyframes that a
// re-parsed track summary actually exposes:
//
//	hits / len(plannedKeyframeSampleNumbers)
//
// A missing index yields zero, never a silent pass.
func KeyframeHitRate(planned []uint32, actual []uint32) float64 {
	if len(planned) == 0 {
		return 0
	}
	present := map[uint32]struct{}{}
	for _, k := range actual {
		present[k] = struct{}{}
	}
	hits := 0
	for _, k := range planned {
		if _, ok := present[k]; ok {
			hits++
		}
	}
	return float64(hits) / float64(len(planned))
}

// PlannedKeyframes derives the expected keyframe sample numbers for a
// track given its sample count and keyframe interval (1-based numbers).
func PlannedKeyframes(sampleCount uint64, every uint32) []uint32 {
	if every == 0 {
		every = 1
	}
	var out []uint32
	for s := uint32(1); uint64(s) <= sampleCount; s += every {
		out = append(out, s)
	}
	return out
}

// RoundTrip re-parses a container and confirms its track-level summary
// matches the values recorded when the segment was planned. This is the
// independent parser check required for the round-trip rate.
type RoundTripExpectation struct {
	TrackID     uint32
	SampleCount uint64
	Keyframes   []uint32
	Width       uint32
	Height      uint32
}

// RoundTripMP4 parses path and checks the first video track against exp.
func RoundTripMP4(path string, exp RoundTripExpectation) error {
	f, err := os.Open(path)
	if err != nil {
		return model.Wrap(model.KindChecksum, "verify", "roundtrip",
			path, err)
	}
	defer f.Close()
	sum, err := ingest.ParseMP4(f, path)
	if err != nil {
		return err
	}
	for _, tr := range sum.Tracks {
		if tr.Kind != "video" || tr.Video == nil ||
			tr.Video.TrackID != exp.TrackID {
			continue
		}
		v := tr.Video
		if v.SampleCount != exp.SampleCount {
			return model.New(model.KindChecksum, "verify", "roundtrip",
				"sample count changed after re-parse")
		}
		if v.Width != exp.Width || v.Height != exp.Height {
			return model.New(model.KindChecksum, "verify", "roundtrip",
				"dimensions changed after re-parse")
		}
		if !sameSet(v.KeyframeIndexes, exp.Keyframes) {
			return model.New(model.KindChecksum, "verify", "roundtrip",
				"keyframe index changed after re-parse")
		}
		return nil
	}
	return model.New(model.KindChecksum, "verify", "roundtrip",
		"expected video track not found")
}

func sameSet(a, b []uint32) bool {
	if len(a) != len(b) {
		return false
	}
	m := map[uint32]int{}
	for _, x := range a {
		m[x]++
	}
	for _, x := range b {
		m[x]--
		if m[x] < 0 {
			return false
		}
	}
	return true
}
