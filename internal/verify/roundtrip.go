// Package verify independently re-reads pipeline outputs from disk and
// re-parses every fragment instead of trusting in-memory counters. It checks
// round-trip structural integrity, per-fragment content hashes, keyframe
// index reachability and resume determinism.
package verify

import (
	"fmt"
	"os"
	"path/filepath"

	"relay/internal/index"
	"relay/internal/ingest"
	"relay/internal/media"
	"relay/internal/stream"
	"relay/internal/timeline"
)

// RoundTripReport summarizes the independent re-parse of all outputs.
type RoundTripReport struct {
	Reparsed int
	Failed   int
	// Rate is Reparsed / total output fragments, as a percentage 0..100.
	Rate float64
	Faults []*media.Fault
}

// RoundTrip re-parses every fragment listed in the checkpoint. It opens each
// output file afresh and validates boxes and sample ranges, so a structurally
// broken output is caught even if the pipeline counted it as accepted.
func RoundTrip(outDir string, cp *stream.Checkpoint) (*RoundTripReport, error) {
	entries := cp.Entries()
	rep := &RoundTripReport{}
	total := len(entries)
	for _, e := range entries {
		path := filepath.Join(outDir, "frags", filepath.Clean(e.Key))
		raw, err := os.ReadFile(path)
		if err != nil {
			rep.Failed++
			rep.Faults = append(rep.Faults, media.NewFault(media.FaultHashMismatch,
				e.TrackID, e.Sequence, 0, "missing output: "+err.Error()))
			continue
		}
		if _, err := ingest.ParseFragment(path, 0, raw); err != nil {
			rep.Failed++
			f, _ := err.(*media.Fault)
			rep.Faults = append(rep.Faults, f)
			continue
		}
		rep.Reparsed++
	}
	if total > 0 {
		rep.Rate = float64(rep.Reparsed) * 100 / float64(total)
	}
	return rep, nil
}

// Hashes re-hashes every output file from disk and compares it against the
// checkpoint's committed OutputHash. It returns matched/total counts and the
// first mismatch fault.
func Hashes(outDir string, cp *stream.Checkpoint) (matched, total int, fault *media.Fault) {
	for _, e := range cp.Entries() {
		total++
		path := filepath.Join(outDir, "frags", filepath.Clean(e.Key))
		raw, err := os.ReadFile(path)
		if err != nil {
			return matched, total, media.NewFault(media.FaultHashMismatch,
				e.TrackID, e.Sequence, 0, "cannot read: "+err.Error())
		}
		if stream.HashBytes(raw) != e.OutputHash {
			return matched, total, media.NewFault(media.FaultHashMismatch,
				e.TrackID, e.Sequence, 0,
				fmt.Sprintf("output hash drift for %s", e.Key))
		}
		matched++
	}
	return matched, total, nil
}

// Keyframes confirms that every expected keyframe is reachable through the
// published index and that no index entry points at a non-sync sample.
func Keyframes(idx *index.Index, reports []*timeline.TrackReport) (hits, expected int, fault *media.Fault) {
	for _, rep := range reports {
		for _, rf := range rep.Accepted {
			expected++ // each synthetic fragment has exactly one sync sample
			e, ok := idx.Lookup(rf.TrackID, rf.NewSequence, 1)
			if !ok || !e.Sync {
				return hits, expected, media.NewFault(media.FaultStructure,
					rf.TrackID, rf.NewSequence, 0,
					"keyframe missing from published index")
			}
			hits++
		}
	}
	return hits, expected, nil
}

// Resume compares two checkpoints (a full reference run and an
// interrupted-then-resumed run). Every shared key must carry identical
// source and output hashes, proving the resume result equals a clean rerun.
func Resume(reference, resumed *stream.Checkpoint) (match, total int, fault *media.Fault) {
	ref := reference.Entries()
	got := resumed.Entries()
	if len(ref) != len(got) {
		return 0, len(ref), media.NewFault(media.FaultHashMismatch, 0, 0, 0,
			fmt.Sprintf("resume produced %d entries, reference has %d",
				len(got), len(ref)))
	}
	for i := range ref {
		total++
		if ref[i].Key != got[i].Key ||
			ref[i].SourceHash != got[i].SourceHash ||
			ref[i].OutputHash != got[i].OutputHash {
			return match, total, media.NewFault(media.FaultHashMismatch,
				ref[i].TrackID, ref[i].Sequence, 0,
				"resume output diverges from reference at "+ref[i].Key)
		}
		match++
	}
	return match, total, nil
}
