package pipeline

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"bcast/demux"
	"bcast/fingerprint"
	"bcast/internal/fixture"
	"bcast/media"
	"bcast/output"
	"bcast/segments"
)

// Verify checks that the delivered files named in the checkpoint still parse,
// carry the same segment content hashes they were fingerprinted with, and
// round-trip their samples. It re-derives hashes from the files on disk rather
// than trusting the stored summary, so a corrupted delivery is caught.
func Verify(cp *Checkpoint) (*Verification, error) {
	summaryEnc, ok := cp.GetMeta("summary")
	if !ok {
		return nil, fmt.Errorf("pipeline: checkpoint has no summary to verify")
	}
	outDir, ok := cp.GetMeta("outdir")
	if !ok {
		return nil, fmt.Errorf("pipeline: checkpoint has no output directory")
	}
	primary, ok := cp.GetMeta("primary")
	if !ok {
		return nil, fmt.Errorf("pipeline: checkpoint has no primary delivery")
	}
	// Re-derive the primary input with the same seed/segments so the on-disk
	// payloads can be compared byte-for-byte during the round trip.
	seed, segmentsN := parseSeedSegments(cp)
	assets, err := fixture.Generate(fixture.Default(seed, segmentsN))
	if err != nil {
		return nil, fmt.Errorf("pipeline: verify regenerate: %w", err)
	}
	prim := &delivery{name: primary, data: assets.Clean}
	f, err := prim.ensureParsed()
	if err != nil {
		return nil, err
	}
	if err := prim.buildIndex(f); err != nil {
		return nil, err
	}
	target := parseTarget(cp)
	segs, err := segments.Cut(&f.File, target)
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(outDir, primary)
	v := &Verification{StoredSummary: summaryEnc, Dir: dir}
	var matchSum float64
	for _, seg := range segs {
		path := filepath.Join(dir, fmt.Sprintf("seg-%03d.mp4", seg.Index))
		rf, err := os.Open(path)
		if err != nil {
			return nil, fmt.Errorf("pipeline: verify open %s: %w", path, err)
		}
		st, err := rf.Stat()
		if err != nil {
			rf.Close()
			return nil, fmt.Errorf("pipeline: verify stat %s: %w", path, err)
		}
		of, err := demux.Parse(rf, st.Size(), path)
		if err != nil && len(of.Samples) == 0 {
			rf.Close()
			return nil, fmt.Errorf("pipeline: verify parse %s: %w", path, err)
		}
		v.Files++
		v.Samples += len(of.Samples)
		// Compare the delivered payloads byte-for-byte with the source and
		// recompute the segment hash from the delivered file itself. A
		// tampered file changes its hash and fails the comparison.
		rep, err := output.Verify(path, prim.reader(), remuxTracks(f, seg))
		if err != nil {
			rf.Close()
			return nil, err
		}
		if rep.MatchRate() < 1 {
			rf.Close()
			return v, fmt.Errorf("pipeline: %s payload mismatch (rate %.3f)", path, rep.MatchRate())
		}
		diskSeg := segmentFromFile(of)
		diskHash, herr := fingerprint.Hash(rf, diskSeg)
		rf.Close()
		wantHash, werr := fingerprint.Hash(prim.reader(), seg)
		if herr != nil || werr != nil || diskHash != wantHash {
			return v, fmt.Errorf("pipeline: %s content hash mismatch", path)
		}
		matchSum += rep.MatchRate()
		v.SegmentHashes++
	}
	if v.Files > 0 {
		v.MatchRate = matchSum / float64(v.Files)
	}
	if v.MatchRate < 0.98 {
		return v, fmt.Errorf("pipeline: round-trip match rate %.3f below 0.98", v.MatchRate)
	}
	return v, nil
}

// segmentFromFile wraps a parsed file's samples as a single segment so the
// fingerprint can be recomputed from the bytes actually on disk.
func segmentFromFile(f *demux.File) segments.Segment {
	seg := segments.Segment{Index: 0}
	for _, s := range f.Samples {
		if s.Kind == media.TrackAudio {
			seg.Audio = append(seg.Audio, s)
		} else {
			seg.Video = append(seg.Video, s)
		}
	}
	return seg
}

// Verification is the outcome of re-checking delivered files.
type Verification struct {
	Dir           string
	Files         int
	Samples       int
	SegmentHashes int
	MatchRate     float64
	StoredSummary string
}

// SummaryLine renders the acceptance metrics lines for a verify run.
func (v *Verification) SummaryLine() string {
	var b strings.Builder
	fmt.Fprintf(&b, "verify_files=%d\n", v.Files)
	fmt.Fprintf(&b, "verify_samples=%d\n", v.Samples)
	fmt.Fprintf(&b, "verify_segment_hashes=%d\n", v.SegmentHashes)
	fmt.Fprintf(&b, "roundtrip_match_rate=%.3f\n", v.MatchRate)
	return b.String()
}

// parseSeedSegments recovers seed and segment count from the stored params
// signature; defaults are used if the fields are missing.
func parseSeedSegments(cp *Checkpoint) (int64, int) {
	p, _ := cp.GetMeta("params")
	seed, segs := int64(17), 12
	for _, part := range strings.Split(p, ";") {
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			continue
		}
		n, err := strconv.ParseInt(kv[1], 10, 64)
		if err != nil {
			continue
		}
		switch kv[0] {
		case "seed":
			seed = n
		case "segments":
			segs = int(n)
		}
	}
	return seed, segs
}

// parseTarget recovers the segment target duration from the stored params.
func parseTarget(cp *Checkpoint) int64 {
	p, _ := cp.GetMeta("params")
	target := int64(90000)
	for _, part := range strings.Split(p, ";") {
		kv := strings.SplitN(part, "=", 2)
		if len(kv) == 2 && kv[0] == "target" {
			if n, err := strconv.ParseInt(kv[1], 10, 64); err == nil {
				target = n
			}
		}
	}
	return target
}
