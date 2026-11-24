package pipeline

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"bcast/fingerprint"
	"bcast/output"
	"bcast/segments"
	"bcast/silence"
)

// Summary carries the metrics printed by the acceptance command. All rates
// are fractions in [0,1]; the printer rounds them for display only.
type Summary struct {
	RoundtripMatchRate float64
	KeyframeHitRate    float64
	SegmentHashes      int
	DamagedRanges      int
	BlockedDependency  string
	PrimaryOK          bool
	Quarantined        int
	RecoveredNodes     int
	ResumedNodes       int
	Segments           int
	SilentWindows      int
	UnknownWindows     int
	OutputFiles        int
	DuplicateGroups    int
	HashDigest         string // digest of the per-segment hash list
	DamageKinds        []string
}

// summarize re-derives every reported metric from the inputs and the files on
// disk. It deliberately does not read transient node fields: on a resumed run
// the nodes holding those fields were reused and never executed, so deriving
// from inputs is what makes repeated runs report identical numbers.
func summarize(ds []*delivery, dag *DAG, p Params) *Summary {
	rep := dag.ExecuteReport()
	sum := &Summary{RecoveredNodes: rep.RecoveredNodes, ResumedNodes: rep.ResumedNodes}
	sum.BlockedDependency = rep.FirstBlockedDep
	sum.PrimaryOK = true
	primary := ds[0].name
	for _, id := range append(append([]string{}, rep.FailedNodes...), rep.BlockedNodes...) {
		if strings.HasSuffix(id, ":"+primary) {
			sum.PrimaryOK = false
		}
	}
	// A quarantined input is one with at least one failed or blocked node;
	// count inputs, not nodes, so one bad delivery reports as one.
	quarantined := map[string]bool{}
	for _, id := range append(append([]string{}, rep.FailedNodes...), rep.BlockedNodes...) {
		if name := deliveryOf(id); name != "" && name != primary {
			quarantined[name] = true
		}
	}
	sum.Quarantined = len(quarantined)
	// Damaged ranges and fault kinds aggregate across every delivery: the
	// damaged variants exist precisely to be counted.
	seen := map[string]bool{}
	for _, d := range ds {
		f, err := d.ensureParsed()
		if f == nil {
			continue
		}
		_ = err // recoverable parse errors still yield a usable prefix
		sum.DamagedRanges += len(f.Damaged)
		for _, r := range f.Damaged {
			seen[r.Reason] = true
		}
		for _, iss := range f.Issues {
			seen[string(iss.Kind)] = true
		}
	}
	for k := range seen {
		sum.DamageKinds = append(sum.DamageKinds, k)
	}
	sort.Strings(sum.DamageKinds)
	// Rates describe the primary (clean) deliverable only.
	prim := ds[0]
	f, err := prim.ensureIndexed()
	if err != nil {
		return sum
	}
	segs, err := segments.Cut(&f.File, p.TargetDur)
	if err != nil {
		return sum
	}
	sum.Segments = len(segs)
	if rep := silence.Detect(&f.File, prim.reader(), p.WindowSize, p.Threshold); rep != nil {
		sum.SilentWindows = rep.SilentWindows
		sum.UnknownWindows = rep.UnknownWindows
	}
	hit, total := keyframeHits(segs)
	if total > 0 {
		sum.KeyframeHitRate = float64(hit) / float64(total)
	}
	var hashes []string
	for _, seg := range segs {
		h, err := fingerprint.Hash(prim.reader(), seg)
		if err != nil {
			continue
		}
		hashes = append(hashes, h)
	}
	sum.SegmentHashes = len(hashes)
	sum.DuplicateGroups = len(fingerprint.Dedup(hashes))
	// A digest of the ordered hash list proves two runs produced identical
	// per-segment fingerprints, not merely the same counts.
	sum.HashDigest = fingerprint.InputDigest(hashes...)
	var rateSum float64
	var files int
	for _, seg := range segs {
		path := filepath.Join(p.OutDir, primary, fmt.Sprintf("seg-%03d.mp4", seg.Index))
		if _, err := os.Stat(path); err != nil {
			continue
		}
		v, err := output.Verify(path, prim.reader(), remuxTracks(f, seg))
		if err != nil {
			continue
		}
		rateSum += v.MatchRate()
		files++
	}
	sum.OutputFiles = files
	if files > 0 {
		sum.RoundtripMatchRate = rateSum / float64(files)
	}
	return sum
}

// deliveryOf extracts the delivery name from a "stage:name" node ID.
func deliveryOf(id string) string {
	if i := strings.IndexByte(id, ':'); i >= 0 {
		return id[i+1:]
	}
	return ""
}

// keyframeHits counts boundaries that landed on a keyframe, excluding the
// opening boundary which is keyframe-aligned by definition.
func keyframeHits(segs []segments.Segment) (hit, total int) {
	for _, s := range segs {
		if s.Boundary.Ideal.Value == 0 {
			continue
		}
		total++
		if s.Boundary.OnKeyframe {
			hit++
		}
	}
	return hit, total
}

// Encode renders the summary as the stable key=value block verify expects.
func (s *Summary) Encode() string {
	var b strings.Builder
	fmt.Fprintf(&b, "roundtrip_match_rate=%.3f\n", s.RoundtripMatchRate)
	fmt.Fprintf(&b, "keyframe_hit_rate=%.3f\n", s.KeyframeHitRate)
	fmt.Fprintf(&b, "segment_hashes=%d\n", s.SegmentHashes)
	fmt.Fprintf(&b, "damaged_ranges=%d\n", s.DamagedRanges)
	fmt.Fprintf(&b, "blocked_dependency=%s\n", s.BlockedDependency)
	fmt.Fprintf(&b, "recovered_nodes=%d\n", s.RecoveredNodes)
	fmt.Fprintf(&b, "segment_hash_digest=%s\n", s.HashDigest)
	return b.String()
}

// plainSummary is the subset of Summary persisted verbatim for verify.
func (s *Summary) plainSummary() string { return s.Encode() }
