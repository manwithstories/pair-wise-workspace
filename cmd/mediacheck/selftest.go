package main

import (
	"fmt"
	"os"
	"path/filepath"

	"mediacheck/internal/fault"
	"mediacheck/internal/fixture"
	"mediacheck/internal/pipeline"
	"mediacheck/internal/stream"
	"mediacheck/internal/timeline"
)

// generateFixtures self-generates every sample the demo needs:
// normal MP4 + VTT, a truncated MP4, bad-timing VTT and an RTP
// out-of-order/loss byte sample.
func generateFixtures(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	spec := fixture.DefaultMP4Spec()
	full := filepath.Join(dir, "generated.mp4")
	if _, err := fixture.WriteMP4(full, spec); err != nil {
		return err
	}
	if err := fixture.WriteVTT(filepath.Join(dir, "generated.vtt"), false); err != nil {
		return err
	}
	if err := fixture.WriteVTT(filepath.Join(dir, "bad.vtt"), true); err != nil {
		return err
	}
	st, err := os.Stat(full)
	if err != nil {
		return err
	}
	if err := fixture.TruncateFile(full, filepath.Join(dir, "trunc.mp4"), st.Size()-30); err != nil {
		return err
	}
	return fixture.WriteRTP(filepath.Join(dir, "rtp.bin"), false)
}

// runSelftest exercises the normal, corrupt and resume paths plus RTP
// reordering, then checks every acceptance metric. Exit 0 = pass.
func runSelftest(tmpdir string, rate timeline.Rate, cfg pipeline.Config) int {
	if err := generateFixtures(tmpdir); err != nil {
		fmt.Fprintln(os.Stderr, "fixture generation:", err)
		return 1
	}
	spec := fixture.DefaultMP4Spec()
	expected := spec.KeyframeTimes()
	mp4 := filepath.Join(tmpdir, "generated.mp4")
	vtt := filepath.Join(tmpdir, "generated.vtt")
	cp := filepath.Join(tmpdir, "run.state")
	os.Remove(cp) // selftest starts from a clean checkpoint

	fail := func(format string, args ...any) int {
		fmt.Fprintf(os.Stderr, "SELFTEST FAIL: "+format+"\n", args...)
		return 1
	}

	// --- Normal path ---
	normal, nf, err := analyze("normal", mp4, vtt, rate, cfg, cp)
	if err != nil {
		return fail("normal path: %v", err)
	}
	fmt.Fprintf(os.Stderr, "[normal] segments=%d cues=%d faults=%v\n",
		normal.SegmentCount, normal.CueCount, nf.ByCategory())

	// Real keyframe hit rate: fixture-expected vs scanned.
	info := scanMP4File(mp4, &fault.List{})
	var actual []int64
	for _, tr := range info.Tracks {
		actual = append(actual, tr.Keyframes...)
	}
	normal.KeyframeHitRate = stream.HitRate(expected, actual, 1_000_000)

	// --- Corrupt path: truncated MP4 + bad captions ---
	corrupt, cf, err := analyze("corrupt", filepath.Join(tmpdir, "trunc.mp4"),
		filepath.Join(tmpdir, "bad.vtt"), rate, cfg, "")
	if err != nil {
		return fail("corrupt path: %v", err)
	}
	fmt.Fprintf(os.Stderr, "[corrupt] corrupt_ranges=%d faults=%v\n",
		len(corrupt.CorruptRanges), cf.ByCategory())
	printReportTo(os.Stderr, corrupt, cf)
	if cf.Len() == 0 {
		return fail("corrupt inputs were not detected")
	}

	// --- Resume path: second run over the same checkpoint ---
	resumed, rf, err := analyze("resume", mp4, vtt, rate, cfg, cp)
	if err != nil {
		return fail("resume path: %v", err)
	}
	fmt.Fprintf(os.Stderr, "[resume] resumed=true segments=%d faults=%v\n",
		resumed.SegmentCount, rf.ByCategory())
	normal.ResumeMatches = resumed.ResumeMatches &&
		pipeline.SegmentHashesEqual(normal.Segments, resumed.Segments)

	// --- RTP reorder/loss sample ---
	rtpData, err := os.ReadFile(filepath.Join(tmpdir, "rtp.bin"))
	if err != nil {
		return fail("read rtp fixture: %v", err)
	}
	rec := stream.NewReorderer(stream.DefaultWindow)
	rfl := &fault.List{}
	stream.ParseRTPStream(rtpData, rec, rfl)
	for _, s := range rec.Stats() {
		fmt.Fprintf(os.Stderr,
			"[rtp] ssrc=%#x received=%d committed=%d reordered=%d late=%d gaps=%d\n",
			s.SSRC, s.Received, s.Committed, s.Reordered, s.Late, s.Gaps)
	}

	// --- Acceptance ---
	printReport(normal, nf)
	switch {
	case normal.RoundtripRate < 0.99:
		return fail("roundtrip_rate %.3f < 0.99", normal.RoundtripRate)
	case normal.KeyframeHitRate < 0.98:
		return fail("keyframe_hit_rate %.3f < 0.98", normal.KeyframeHitRate)
	case normal.SegmentCount < 3:
		return fail("segment_count %d < 3", normal.SegmentCount)
	case !normal.ResumeMatches:
		return fail("resume_matches is false")
	case normal.PeakBufferBytes > normal.PeakBufferBudget:
		return fail("peak buffer %d exceeds budget %d",
			normal.PeakBufferBytes, normal.PeakBufferBudget)
	}
	for _, s := range normal.Segments {
		if len(s.SHA256) != 64 {
			return fail("segment %d sha256 %q not 64 hex chars", s.Index, s.SHA256)
		}
	}
	fmt.Fprintln(os.Stderr, "SELFTEST PASS")
	return 0
}
