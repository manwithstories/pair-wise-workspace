package pipeline

import (
	"strings"
	"testing"

	"mediascan/internal/fixture"
	"mediascan/internal/media"
	"mediascan/internal/subtitle"
)

// demoInput builds the same asset mix the CLI demo uses, at a smaller scale.
func demoInput(t *testing.T, opts demoShape) Input {
	t.Helper()
	const frames = 120
	sizes := make([]uint32, frames)
	for i := range sizes {
		sizes[i] = uint32(600 + (i%7)*40)
	}
	sync := make([]int, 0, frames/10)
	for n := 1; n <= frames; n += 10 {
		sync = append(sync, n)
	}
	markers := append([]int(nil), sync...)
	declared := append([]int(nil), sync...)
	if opts.dropIndex > 0 {
		if opts.dropIndex >= len(declared) {
			declared = nil
		} else {
			declared = declared[:len(declared)-opts.dropIndex]
		}
	}
	corruptIdx := 15
	video := fixture.TrackSpec{
		Kind: "video", Timescale: 30000, SampleDelta: 1001,
		SampleSizes: sizes, SyncSamples: declared, KeyframeMarkers: markers,
		CorruptSizeIdx: corruptIdx,
	}
	// Audio: ~4s at 48kHz/1024 samples per frame (188 frames), real payload
	// sizes, all-sync. This keeps the audio track spanning the whole timeline
	// and free of the corruption injected into the video track.
	const audioFrames = 188
	audioSizes := make([]uint32, audioFrames)
	for i := range audioSizes {
		audioSizes[i] = 128
	}
	audio := fixture.TrackSpec{
		Kind: "audio", Timescale: 48000, SampleDelta: 1024,
		SampleSizes: audioSizes, NoSTSS: true, CorruptSizeIdx: -1,
	}
	mp4, err := fixture.MP4([]fixture.TrackSpec{video, audio})
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	if opts.truncate > 0 && opts.truncate < len(mp4) {
		mp4 = mp4[:len(mp4)-opts.truncate]
	}
	subs := []byte(fixture.SRT(6, 500_000_000, media.NanosPerSecond, 700_000_000))
	return Input{
		SubtitleBytes: subs, SubtitleFormat: subtitle.FormatSRT, SubtitleTrackID: 100,
		MP4Bytes: mp4, RTPPackets: rtpPackets(8), RTPClockRate: 90000,
	}
}

type demoShape struct {
	truncate  int
	dropIndex int
}

func rtpPackets(n int) [][]byte {
	out := make([][]byte, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, fixture.RTPPacket(uint16(65530+i), uint32(i)*3000, 160))
	}
	return out
}

func TestRunNormalPath(t *testing.T) {
	out, err := Run(demoInput(t, demoShape{}), Config{})
	if err != nil {
		t.Fatal(err)
	}
	if out.SubtitleRoundtrip != 1.0 {
		t.Errorf("roundtrip = %v", out.SubtitleRoundtrip)
	}
	if out.KeyframeHitRate < 0.98 {
		t.Errorf("keyframe hit rate = %v", out.KeyframeHitRate)
	}
	if out.Fatal {
		t.Errorf("normal path fatal: %v", out.Issues)
	}
	if len(out.Segments) == 0 {
		t.Fatal("no segments produced")
	}
	// Corrupt sample is isolated to a locatable range, not a whole-track fail.
	if len(out.Corrupt) == 0 {
		t.Error("baseline corrupt sample not located")
	}
	if out.ExitCode() != 2 {
		t.Errorf("exit code = %d, want 2 (localized recoverable fault)", out.ExitCode())
	}
}

func TestRunDeterministicAcrossRuns(t *testing.T) {
	in := demoInput(t, demoShape{})
	a, err := Run(in, Config{})
	if err != nil {
		t.Fatal(err)
	}
	b, err := Run(in, Config{})
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Segments) != len(b.Segments) {
		t.Fatalf("segment counts differ: %d vs %d", len(a.Segments), len(b.Segments))
	}
	for i := range a.Segments {
		if a.Segments[i].Hash != b.Segments[i].Hash {
			t.Errorf("segment %d hash differs between identical runs", i)
		}
	}
}

// TestPropagationBadSampleRebuildsIndex is the required cross-module path:
// a lost stss index detected from sample bytes must flow through the stream
// analyzer into an index rebuild, not a track failure.
func TestPropagationBadSampleRebuildsIndex(t *testing.T) {
	out, err := Run(demoInput(t, demoShape{dropIndex: 2}), Config{})
	if err != nil {
		t.Fatal(err)
	}
	if out.RebuiltIndexes == 0 {
		t.Fatalf("index was not rebuilt; issues: %v", out.Issues)
	}
	if !hasClass(out.Issues, media.ClassMP4MissingIndex) {
		t.Errorf("missing-index class not propagated: %v", out.Issues)
	}
	if !hasClass(out.Issues, media.ClassMP4IndexIncomplete) {
		t.Errorf("index-incomplete class not propagated: %v", out.Issues)
	}
	// The rebuild must still achieve the hit-rate target and keep the run
	// alive rather than failing the track.
	if out.KeyframeHitRate < 0.98 {
		t.Errorf("hit rate after rebuild = %v", out.KeyframeHitRate)
	}
	if out.Fatal {
		t.Errorf("rebuild path reported fatal: %v", out.Issues)
	}
	if len(out.Segments) == 0 {
		t.Error("rebuild path produced no segments")
	}
}

// TestPropagationLateTimecodeReplaysCommitted is the second required
// cross-module path: an event that arrives after the watermark must replay
// committed segments and still match the canonical run exactly.
func TestPropagationLateTimecodeReplaysCommitted(t *testing.T) {
	in := demoInput(t, demoShape{})
	out, err := RunReplayed(in, Config{}, 3)
	if err != nil {
		t.Fatal(err)
	}
	if out.ReplayCount == 0 {
		t.Fatal("late events did not trigger a replay")
	}
	if !out.ReplayEqual {
		t.Errorf("replay diverged from canonical run; issues: %v", out.Issues)
	}
	if !hasClass(out.Issues, media.ClassTimelineLateEvent) {
		t.Errorf("late-event class missing: %v", out.Issues)
	}
	// Compare against a plain run: hashes must be identical for the replay to
	// be trustworthy.
	plain, err := Run(in, Config{})
	if err != nil {
		t.Fatal(err)
	}
	if len(plain.Segments) != len(out.Segments) {
		t.Fatalf("segment counts differ: plain %d, replayed %d", len(plain.Segments), len(out.Segments))
	}
	for i := range plain.Segments {
		if plain.Segments[i].Hash != out.Segments[i].Hash {
			t.Errorf("segment %d: replayed hash %s != canonical %s", i,
				out.Segments[i].Hash, plain.Segments[i].Hash)
		}
	}
}

func TestRunTruncatedIsolatesDamage(t *testing.T) {
	out, err := Run(demoInput(t, demoShape{truncate: 17}), Config{})
	if err != nil {
		t.Fatal(err)
	}
	if out.Fatal {
		t.Errorf("truncation reported fatal: %v", out.Issues)
	}
	if len(out.Corrupt) == 0 {
		t.Error("truncation produced no corrupt range")
	}
	// Both tracks still contribute samples despite the damaged tail.
	if out.Tracks != 2 {
		t.Errorf("tracks = %d, want 2", out.Tracks)
	}
	if len(out.Segments) == 0 {
		t.Error("no segments after truncation")
	}
}

// TestDamageDoesNotLeakAcrossTracks asserts the isolation guarantee: with a
// corrupt sample on the video track, the audio track's own samples remain
// clean and both tracks are still present in every committed segment's track
// set is not required, but no corrupt range may be attributed to the audio
// track.
func TestDamageDoesNotLeakAcrossTracks(t *testing.T) {
	in := demoInput(t, demoShape{})
	out, err := Run(in, Config{})
	if err != nil {
		t.Fatal(err)
	}
	// The baseline fixture corrupts exactly one video sample (track 1).
	for _, r := range out.Corrupt {
		if r.TrackID != 1 {
			t.Errorf("corruption attributed to track %d, want only track 1", r.TrackID)
		}
	}
	// The run still produced segments spanning the full timeline, proving the
	// audio track and undamaged video GOPs continued to aggregate. The video
	// track is 120 frames at 29.97 (~4s), so at the default 1s segment size we
	// expect at least 4 segments.
	if len(out.Segments) < 4 {
		t.Errorf("only %d segments; damage was not isolated", len(out.Segments))
	}
	// The damaged sample lies in the first segment only; later segments must
	// be clean.
	if out.Segments[0].Recovery == "clean" {
		t.Error("segment 0 holding the corrupt sample reported clean")
	}
	for i := 1; i < len(out.Segments); i++ {
		if out.Segments[i].Recovery != "clean" {
			t.Errorf("segment %d tainted by damage outside its span", i)
		}
	}
}

func hasClass(set media.IssueSet, c media.Class) bool {
	for _, is := range set {
		if is.Class == c {
			return true
		}
	}
	return false
}

func TestSegmentsCarryTimecodes(t *testing.T) {
	out, err := Run(demoInput(t, demoShape{}), Config{VideoFrameRate: media.Rate2997})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Segments) == 0 {
		t.Fatal("no segments")
	}
	for _, s := range out.Segments {
		if s.StartTimecode == "" {
			t.Errorf("segment %d has no timecode", s.Index)
		}
		// Drop-frame labels use ';' before the frame field.
		if !strings.Contains(s.StartTimecode, ";") {
			t.Errorf("segment %d timecode %q is not drop-frame", s.Index, s.StartTimecode)
		}
	}
	// 1-second boundaries at 30000/1001 are not frame aligned, which must be
	// reported rather than hidden.
	if out.TimecodeNonIntegral == 0 {
		t.Error("expected non-integral frame times at 29.97 with 1s segments")
	}
}
