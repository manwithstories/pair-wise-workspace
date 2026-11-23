package report

import (
	"strings"
	"testing"

	"mediascan/internal/fault"
)

func baseParams() Params {
	return Params{Seed: 42, Tracks: 4, Frames: 1200, CorruptAt: 731,
		Timescale: 30_000, MaxKeyGap: 250_000, DriftLimit: 50_000}
}

func sampleFaults() []fault.Fault {
	return []fault.Fault{
		fault.New(fault.KindTruncatedBox, fault.SeverityFatal, 1, 731, 4096, 24_000, "trunc"),
		fault.New(fault.KindKeyframeGap, fault.SeverityWarning, 2, 80, 512, 2_600, "gap"),
		fault.New(fault.KindClockDrift, fault.SeverityWarning, 3, 5, -1, 9_000, "drift"),
	}
}

func TestDigestDeterministic(t *testing.T) {
	p := baseParams()
	fs := sampleFaults()
	d1 := Digest(p, fs)
	d2 := Digest(p, fs)
	if d1 != d2 || len(d1) != 64 {
		t.Fatalf("digest not stable: %q vs %q", d1, d2)
	}
	// Feeding equal-position faults in a different order but merging first
	// must give the same hash because the canonical order removes the
	// scheduling-dependent input order.
	samePos := []fault.Fault{
		fault.New(fault.KindBadSample, fault.SeverityError, 1, 10, 100, 500, "a"),
		fault.New(fault.KindKeyframeGap, fault.SeverityWarning, 1, 10, 100, 500, "b"),
		fault.New(fault.KindClockDrift, fault.SeverityWarning, 1, 10, 100, 500, "c"),
	}
	forward := mergeForTest(append([]fault.Fault(nil), samePos...))
	reverse := mergeForTest([]fault.Fault{samePos[2], samePos[1], samePos[0]})
	if Digest(p, forward) != Digest(p, reverse) {
		t.Fatal("digest must be order-independent after canonical merge")
	}
}

func TestDigestSensitiveToParamsAndFaults(t *testing.T) {
	p := baseParams()
	fs := sampleFaults()
	base := Digest(p, fs)

	p2 := p
	p2.Seed = 43
	if Digest(p2, fs) == base {
		t.Fatal("changing seed must change digest")
	}
	if Digest(p, fs[:1]) == base {
		t.Fatal("changing faults must change digest")
	}
	p3 := p
	p3.Frames = 1201
	if Digest(p3, fs) == base {
		t.Fatal("changing frames must change digest")
	}
}

func TestBuildSummaryAndRender(t *testing.T) {
	p := baseParams()
	fs := sampleFaults()
	s := Build(p, 4, 1200, fs, 0, 0)
	if s.Tracks != 4 || s.Frames != 1200 {
		t.Fatalf("header wrong: %+v", s)
	}
	// Earliest frame among faults is 5 (drift), but truncation offset 4096
	// must still be surfaced.
	if s.FirstBadFrame != 5 {
		t.Fatalf("first bad frame=%d want 5", s.FirstBadFrame)
	}
	if s.OffsetBytes != 4096 {
		t.Fatalf("offset=%d want 4096", s.OffsetBytes)
	}
	if len(s.SHA256) != 64 {
		t.Fatalf("hash length wrong: %q", s.SHA256)
	}
	out := Render(s)
	if !strings.Contains(out, "tracks=4 frames=1200") {
		t.Fatalf("render missing header:\n%s", out)
	}
	if !strings.Contains(out, "sha256=") {
		t.Fatalf("render missing sha:\n%s", out)
	}
}

func TestResumeRender(t *testing.T) {
	s := Build(baseParams(), 4, 1200, nil, 137, 680)
	out := Render(s)
	if !strings.Contains(out, "resume_ms=137") ||
		!strings.Contains(out, "recomputed_frames=680") {
		t.Fatalf("resume render wrong:\n%s", out)
	}
	// A clean run reports first_bad_frame as absent.
	if strings.Contains(out, "first_bad_frame") {
		t.Fatalf("clean run must not report a bad frame:\n%s", out)
	}
}

// mergeForTest mirrors pipeline.MergeFaults without importing pipeline
// (keeps the report package dependency-free).
func mergeForTest(fs []fault.Fault) []fault.Fault {
	out := append([]fault.Fault(nil), fs...)
	// simple insertion sort using the canonical order key
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && fault.OrderKey(out[j-1], out[j]) > 0; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}
