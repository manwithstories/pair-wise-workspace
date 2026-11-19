package demux

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mediapack/internal/fault"
	"mediapack/internal/fixture"
)

func contains(s, sub string) bool { return strings.Contains(s, sub) }

func genFixtures(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := fixture.Generate(dir); err != nil {
		t.Fatalf("generate fixtures: %v", err)
	}
	return dir
}

func TestOpenValid(t *testing.T) {
	dir := genFixtures(t)
	c, err := Open(filepath.Join(dir, "valid.mov"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Faults.MaxSeverity() > fault.Recoverable {
		t.Fatalf("unexpected faults: %v", c.Faults.Faults)
	}
	if len(c.Tracks) != 3 {
		t.Fatalf("tracks: %d", len(c.Tracks))
	}
	v := c.VideoTracks()
	if len(v) != 2 {
		t.Fatalf("video tracks: %d", len(v))
	}
	for _, tr := range v {
		if len(tr.Samples) != 300 {
			t.Fatalf("track %d samples: %d", tr.ID, len(tr.Samples))
		}
		kfs := 0
		for _, s := range tr.Samples {
			if s.Keyframe {
				kfs++
			}
			if !s.Available {
				t.Fatalf("track %d sample %d unavailable", tr.ID, s.Index)
			}
		}
		if kfs != 10 {
			t.Fatalf("track %d keyframes: %d", tr.ID, kfs)
		}
		if !tr.IndexComplete {
			t.Fatal("index should be complete")
		}
	}
	if len(c.AudioTracks()) != 1 {
		t.Fatal("expected one audio track")
	}
	if len(c.Missing) != 0 {
		t.Fatalf("missing intervals: %v", c.Missing)
	}
}

func TestOpenTruncated(t *testing.T) {
	dir := genFixtures(t)
	c, err := Open(filepath.Join(dir, "truncated.mov"))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Missing) == 0 {
		t.Fatal("expected missing intervals")
	}
	if c.Faults.MaxSeverity() < fault.Skip {
		t.Fatal("expected skip-severity faults")
	}
	// The gap must be locatable: at least one fault carries a media time.
	found := false
	for _, f := range c.Faults.Faults {
		if f.MediaTicks >= 0 {
			found = true
		}
	}
	if !found {
		t.Fatal("no fault carries a media time")
	}
}

func TestOpenNoIndex(t *testing.T) {
	dir := genFixtures(t)
	c, err := Open(filepath.Join(dir, "noindex.mov"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tr := range c.VideoTracks() {
		if tr.IndexComplete {
			t.Fatal("index should be incomplete")
		}
		kfs := 0
		for _, s := range tr.Samples {
			if s.Keyframe {
				kfs++
			}
		}
		if kfs != 10 {
			t.Fatalf("recovered keyframes: %d", kfs)
		}
	}
}

func TestOpenCorrupt(t *testing.T) {
	dir := genFixtures(t)
	c, err := Open(filepath.Join(dir, "corrupt.mov"))
	if err != nil {
		t.Fatal(err)
	}
	var rollback, zeroSize bool
	for _, f := range c.Faults.Faults {
		if f.Severity != fault.Skip {
			continue
		}
		if contains(f.Message, "rollback") {
			rollback = true
		}
		if contains(f.Message, "zero-size") {
			zeroSize = true
		}
	}
	if !rollback || !zeroSize {
		t.Fatalf("expected rollback and zero-size faults: %v", c.Faults.Faults)
	}
	if c.Faults.Blocking(fault.PolicyStrict) != true {
		t.Fatal("strict policy must block on corrupt input")
	}
	if c.Faults.Blocking(fault.PolicySkip) != false {
		t.Fatal("skip policy must tolerate corrupt input")
	}
}

func TestOpenEmpty(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "empty.mov")
	if err := os.WriteFile(p, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Faults.MaxSeverity() != fault.Fatal {
		t.Fatal("empty file must be fatal")
	}
}

func TestOpenMissing(t *testing.T) {
	if _, err := Open(filepath.Join(t.TempDir(), "nope.mov")); err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestBuildSamplesTable(t *testing.T) {
	fl := &fault.List{}
	in := tableInput{
		stts: []sttsEntry{{Count: 4, Delta: 3000}},
		ctts: []cttsEntry{{Count: 4, Offset: 3000}},
		stsc: []stscEntry{{FirstChunk: 1, SamplesPerChunk: 4, DescIndex: 1}},
		stsz: []uint32{100, 100, 100, 100},
		stco: []uint64{1000},
		stss: map[uint32]bool{1: true},
	}
	samples := buildSamples(in, fl, 1, 90000, 10000)
	if len(samples) != 4 {
		t.Fatalf("samples: %d", len(samples))
	}
	if samples[1].Offset != 1100 || samples[3].Offset != 1300 {
		t.Fatalf("offsets: %d %d", samples[1].Offset, samples[3].Offset)
	}
	if samples[2].PTS != 9000 {
		t.Fatalf("pts: %d", samples[2].PTS)
	}
	if !samples[0].Keyframe || samples[1].Keyframe {
		t.Fatal("keyframe flags wrong")
	}
	if len(fl.Faults) != 0 {
		t.Fatalf("faults: %v", fl.Faults)
	}
}

func TestBuildSamplesDetectsDamage(t *testing.T) {
	fl := &fault.List{}
	in := tableInput{
		stts: []sttsEntry{{Count: 3, Delta: 3000}},
		ctts: []cttsEntry{{Count: 1, Offset: 0}, {Count: 1, Offset: -9000}, {Count: 1, Offset: 0}},
		stsc: []stscEntry{{FirstChunk: 1, SamplesPerChunk: 3, DescIndex: 1}},
		stsz: []uint32{100, 0, 100},
		stco: []uint64{1000},
	}
	samples := buildSamples(in, fl, 1, 90000, 10000)
	if len(fl.Faults) < 2 {
		t.Fatalf("expected rollback and zero-size faults: %v", fl.Faults)
	}
	// sample 1: zero size and PTS rollback (PTS -6000 < previous 0).
	if samples[1].Available {
		t.Fatal("sample 1 must be unavailable")
	}
	// sample 2 recovers: PTS 6000 > previous -6000.
	if !samples[2].Available {
		t.Fatal("sample 2 must be available")
	}
}

func TestBuildSamplesTruncation(t *testing.T) {
	fl := &fault.List{}
	in := tableInput{
		stts: []sttsEntry{{Count: 3, Delta: 3000}},
		stsc: []stscEntry{{FirstChunk: 1, SamplesPerChunk: 3, DescIndex: 1}},
		stsz: []uint32{100, 100, 100},
		stco: []uint64{1000},
	}
	samples := buildSamples(in, fl, 1, 90000, 1150)
	if !samples[0].Available {
		t.Fatal("sample 0 fits in 1150 bytes")
	}
	if samples[1].Available || samples[2].Available {
		t.Fatal("samples 1 and 2 exceed the file size")
	}
	ivs := missingIntervals(samples, 90000)
	if len(ivs) != 1 {
		t.Fatalf("expected one merged missing interval, got %v", ivs)
	}
}
