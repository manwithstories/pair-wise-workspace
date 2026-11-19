package fixture

import (
	"testing"

	"pairflow/media/internal/clock"
	"pairflow/media/internal/mp4"
	"pairflow/media/internal/subtitle"
)

func TestGenerateIsDeterministic(t *testing.T) {
	opts := DefaultOptions()
	a, err := Generate(opts)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Generate(opts)
	if err != nil {
		t.Fatal(err)
	}
	if string(a.Master) != string(b.Master) {
		t.Fatal("MP4 output not deterministic")
	}
	if string(a.SRT) != string(b.SRT) || string(a.VTT) != string(b.VTT) {
		t.Fatal("subtitle output not deterministic")
	}
	if len(a.RTP) != len(b.RTP) {
		t.Fatal("RTP length differs")
	}
	for i := range a.RTP {
		if a.RTP[i] != b.RTP[i] {
			t.Fatalf("RTP packet %d differs", i)
		}
	}
}

func TestGeneratedSubtitleCorpora(t *testing.T) {
	set, err := Generate(DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	srt := subtitle.ParseSRT(set.SRT, "sub-srt")
	if len(srt.Cues) != 4 {
		t.Fatalf("SRT cues = %d, want 4", len(srt.Cues))
	}
	if len(srt.Errors) < 2 {
		t.Fatalf("SRT errors = %+v, want >= 2", srt.Errors)
	}
	// The overlapping pair must survive: cue 2 starts before cue 1 ends.
	if !srt.Cues[1].Start.LT(srt.Cues[0].End) {
		t.Fatal("overlap not preserved")
	}
	vtt := subtitle.ParseVTT(set.VTT, "sub-vtt")
	if len(vtt.Cues) != 2 {
		t.Fatalf("VTT cues = %d", len(vtt.Cues))
	}
	if !vtt.BOM || !vtt.CRLF {
		t.Fatalf("VTT flags bom=%v crlf=%v", vtt.BOM, vtt.CRLF)
	}
	if len(vtt.Errors) != 1 || vtt.Errors[0].Code != "bad_timestamp" {
		t.Fatalf("VTT errors = %+v", vtt.Errors)
	}
}

func TestGeneratedRTPWraps(t *testing.T) {
	set, err := Generate(DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	wrapped := false
	for i := 1; i < len(set.RTP); i++ {
		if set.RTP[i].Timestamp < set.RTP[i-1].Timestamp {
			wrapped = true
		}
	}
	if !wrapped {
		t.Fatal("RTP timestamps never crossed the 32-bit boundary")
	}
	if set.RTP[0].Timestamp != set.Options.WrapAt {
		t.Fatalf("first timestamp = %d, want %d", set.RTP[0].Timestamp, set.Options.WrapAt)
	}
	// Node timing must advance monotonically and drift upwards.
	for i := 1; i < len(set.RTP); i++ {
		if set.RTP[i].NodeTicks <= set.RTP[i-1].NodeTicks {
			t.Fatalf("node ticks not monotonic at %d", i)
		}
	}
}

func TestGeneratedMP4Variants(t *testing.T) {
	set, err := Generate(DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	master, err := mp4.ParseBytes("master.mp4", set.Master)
	if err != nil {
		t.Fatal(err)
	}
	if len(master.Errors) != 0 {
		t.Fatalf("master parse errors: %v", master.Errors)
	}
	if len(master.Tracks) != 2 {
		t.Fatalf("master tracks = %d", len(master.Tracks))
	}
	if !master.Tracks[0].VFR {
		t.Fatal("master video should be VFR")
	}
	vfrSamples := 0
	for _, s := range master.Tracks[0].Samples {
		if !s.Sync {
			vfrSamples++
		}
	}
	if vfrSamples < 10 {
		t.Fatalf("non-sync samples = %d", vfrSamples)
	}

	trunc, err := mp4.ParseBytes("trunc.mp4", set.TruncatedMP4)
	if err != nil {
		t.Fatal(err)
	}
	if trunc.Tracks[0].Missing == 0 {
		t.Fatal("truncated stsz not detected")
	}

	dup, err := mp4.ParseBytes("dup.mp4", set.DuplicatePTS)
	if err != nil {
		t.Fatal(err)
	}
	duplicates := 0
	for _, e := range dup.Errors {
		if e.Code == "duplicate_pts" {
			duplicates++
		}
	}
	if duplicates == 0 {
		t.Fatalf("duplicate PTS not reported: %+v", dup.Errors)
	}

	oob, err := mp4.ParseBytes("oob.mp4", set.OutOfBounds)
	if err != nil {
		t.Fatal(err)
	}
	if oob.Tracks[0].OutOfBounds == 0 {
		t.Fatal("out-of-bounds samples not detected")
	}
}

func TestDriftEstimateFromFixture(t *testing.T) {
	set, err := Generate(DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	pairs := make([]clock.Pair, 0, len(set.RTP))
	for _, p := range set.RTP {
		pairs = append(pairs, clock.Pair{SourceTicks: p.SourceTicks, NodeTicks: p.NodeTicks})
	}
	base := clock.NewBase(1, 90000)
	corr, anomalies, ok := clock.Estimate(pairs, base, clock.NewRational(1, 100))
	if !ok || corr.Samples != len(pairs) {
		t.Fatalf("estimate failed: %+v", corr)
	}
	if len(anomalies) != 0 {
		t.Fatalf("unexpected anomalies: %+v", anomalies)
	}
	drift := corr.DriftPPM.Seconds()
	if drift < 80 || drift > 90 {
		t.Fatalf("estimated drift = %.2f ppm, want ~85", drift)
	}
}
