package fixture

import (
	"testing"

	"github.com/fieldcut/fieldcut/internal/base"
	"github.com/fieldcut/fieldcut/internal/ingest"
)

// scanOne runs the correct scanner for a generated stream's codec.
func scanOne(t *testing.T, bs *BareStream) ([]*ingest.AccessUnit, ingest.ScanStats) {
	t.Helper()
	clock := base.NewClock()
	noFault := func(*base.Fault) {}
	switch bs.Params.Codec {
	case ingest.CodecH264:
		out, stats, err := ingest.ScanH264(bs.Manifest, bs.Data, clock, noFault)
		if err != nil {
			t.Fatal(err)
		}
		return out, stats
	case ingest.CodecH265:
		out, stats, err := ingest.ScanH265(bs.Manifest, bs.Data, clock, noFault)
		if err != nil {
			t.Fatal(err)
		}
		return out, stats
	case ingest.CodecAV1:
		out, stats, err := ingest.ScanAV1(bs.Manifest, bs.Data, clock, noFault)
		if err != nil {
			t.Fatal(err)
		}
		return out, stats
	}
	return nil, ingest.ScanStats{}
}

func TestMarkedKeyframes(t *testing.T) {
	t.Parallel()
	bs := Generate(Params{Camera: "c", Rendition: "r", Codec: ingest.CodecH264,
		Units: 720, TimeBase: base.ClockRate, Seed: 1})
	marked := 0
	for _, e := range bs.Manifest.Entries {
		if e.Keyframe {
			marked++
		}
	}
	if marked != 20 {
		t.Fatalf("expected 20 marked keyframes, got %d", marked)
	}
}

func TestTruncationDetectedForEveryCodec(t *testing.T) {
	t.Parallel()
	codecs := []ingest.Codec{ingest.CodecH264, ingest.CodecH265, ingest.CodecAV1}
	for _, codec := range codecs {
		codec := codec
		t.Run(string(codec), func(t *testing.T) {
			bs := Generate(Params{Camera: "c", Rendition: "r", Codec: codec,
				Units: 720, TimeBase: base.ClockRate, Seed: 47})
			ApplyInjections(bs, []string{InjectTruncAU})
			_, stats := scanOne(t, bs)
			if stats.Truncated == 0 {
				t.Fatalf("%s: expected a truncated unit", codec)
			}
			if stats.Recovered == 0 {
				t.Fatalf("%s: expected resynchronization", codec)
			}
			// Exactly the corrupted marked keyframe is missing from hits.
			if stats.KeyframeHits != stats.IndexKeyframes-1 {
				t.Fatalf("%s: hits %d != claims-1 (%d)", codec,
					stats.KeyframeHits, stats.IndexKeyframes-1)
			}
		})
	}
}

func TestIndexDriftTriggersRescan(t *testing.T) {
	t.Parallel()
	bs := Generate(Params{Camera: "c", Rendition: "r", Codec: ingest.CodecH265,
		Units: 720, TimeBase: base.ClockRate, Seed: 5})
	ApplyInjections(bs, []string{InjectIndexDrift})
	_, stats := scanOne(t, bs)
	if stats.Rescans == 0 {
		t.Fatal("expected a bounded forward rescan")
	}
}

func TestClockWrapStaysMonotonic(t *testing.T) {
	t.Parallel()
	bs := Generate(Params{Camera: "c", Rendition: "r", Codec: ingest.CodecH264,
		Units: 720, TimeBase: base.ClockRate, Seed: 9})
	ApplyInjections(bs, []string{InjectClockWrap})
	out, _ := scanOne(t, bs)
	if len(out) < 2 {
		t.Fatal("too few units after wrap")
	}
	for i := 1; i < len(out); i++ {
		if out[i].PTS90k <= out[i-1].PTS90k {
			t.Fatalf("non-monotonic PTS at %d after clock wrap", i)
		}
	}
	// The unwrapped timeline must stay within one program length, not blow
	// up by multiples of the 33-bit modulus.
	if out[len(out)-1].PTS90k > base.ClockRate*60 {
		t.Fatalf("output timeline inflated by wrap: %d", out[len(out)-1].PTS90k)
	}
}

func TestColorConflictProducesThreeSources(t *testing.T) {
	t.Parallel()
	bs := Generate(Params{Camera: "c", Rendition: "r", Codec: ingest.CodecAV1,
		Units: 60, TimeBase: base.ClockRate, Seed: 3})
	ApplyInjections(bs, []string{InjectColorConflict})
	sources := map[string]bool{}
	for _, tag := range bs.ColorTags {
		sources[tag.Source] = true
	}
	if !sources["policy"] || !sources["manifest"] || !sources["bitstream"] {
		t.Fatalf("expected three conflicting color sources, got %v", sources)
	}
}

func TestDeterministicGeneration(t *testing.T) {
	t.Parallel()
	a := Generate(Params{Camera: "c", Rendition: "r", Codec: ingest.CodecH264,
		Units: 100, TimeBase: base.ClockRate, Seed: 99})
	b := Generate(Params{Camera: "c", Rendition: "r", Codec: ingest.CodecH264,
		Units: 100, TimeBase: base.ClockRate, Seed: 99})
	if string(a.Data) != string(b.Data) {
		t.Fatal("same seed must produce byte-identical streams")
	}
}
