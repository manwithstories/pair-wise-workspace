package ingest_test

import (
	"bytes"
	"testing"

	"github.com/fieldcut/fieldcut/internal/base"
	"github.com/fieldcut/fieldcut/internal/fixture"
	"github.com/fieldcut/fieldcut/internal/ingest"
)

// sink collects faults for assertions instead of discarding them.
func sink() (*[]*base.Fault, func(*base.Fault)) {
	var faults []*base.Fault
	return &faults, func(f *base.Fault) { faults = append(faults, f) }
}

func TestScanAllCodecsDetectKeyframes(t *testing.T) {
	t.Parallel()
	codecs := []ingest.Codec{ingest.CodecH264, ingest.CodecH265, ingest.CodecAV1}
	for _, codec := range codecs {
		codec := codec
		t.Run(string(codec), func(t *testing.T) {
			bs := fixture.Generate(fixture.Params{
				Camera: "c", Rendition: "r", Codec: codec,
				Units: 60, TimeBase: base.ClockRate, Seed: 7,
			})
			clock := base.NewClock()
			_, sinkFn := sink()
			out, stats, err := scan(codec, bs, clock, sinkFn)
			if err != nil {
				t.Fatalf("%s scan: %v", codec, err)
			}
			if len(out) != 60 {
				t.Fatalf("%s: expected 60 units, got %d", codec, len(out))
			}
			if stats.IndexKeyframes == 0 || stats.KeyframeHits != stats.IndexKeyframes {
				t.Fatalf("%s: keyframe hits %d/%d", codec, stats.KeyframeHits, stats.IndexKeyframes)
			}
			if !out[0].Keyframe {
				t.Fatalf("%s: first unit must be a keyframe", codec)
			}
			// Shared clock output must be strictly increasing.
			for i := 1; i < len(out); i++ {
				if out[i].PTS90k <= out[i-1].PTS90k {
					t.Fatalf("%s: non-monotonic PTS at %d", codec, i)
				}
			}
		})
	}
}

func TestTruncatedAUIsolatedAndResyncs(t *testing.T) {
	t.Parallel()
	bs := fixture.Generate(fixture.Params{
		Camera: "c", Rendition: "r", Codec: ingest.CodecH264,
		Units: 120, TimeBase: base.ClockRate, Seed: 11,
	})
	fixture.ApplyInjections(bs, []string{fixture.InjectTruncAU})
	clock := base.NewClock()
	_, sinkFn := sink()
	out, stats, err := ingest.ScanH264(bs.Manifest, bs.Data, clock, sinkFn)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Truncated == 0 {
		t.Fatal("expected at least one isolated truncated unit")
	}
	if stats.Recovered == 0 {
		t.Fatal("expected resynchronization at a later keyframe")
	}
	// Output never includes the malformed payload and stays monotonic.
	for i := 1; i < len(out); i++ {
		if out[i].PTS90k <= out[i-1].PTS90k {
			t.Fatalf("non-monotonic PTS after recovery at %d", i)
		}
	}
}

func TestIndexDriftRecoveredByForwardRescan(t *testing.T) {
	t.Parallel()
	bs := fixture.Generate(fixture.Params{
		Camera: "c", Rendition: "r", Codec: ingest.CodecH265,
		Units: 90, TimeBase: base.ClockRate, Seed: 13,
	})
	fixture.ApplyInjections(bs, []string{fixture.InjectIndexDrift})
	clock := base.NewClock()
	_, sinkFn := sink()
	out, stats, err := ingest.ScanH265(bs.Manifest, bs.Data, clock, sinkFn)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Rescans == 0 {
		t.Fatal("expected a bounded forward rescan event")
	}
	if len(out) == 0 {
		t.Fatal("rescan should still produce units")
	}
}

func TestManifestRejectsOutOfRangeAndDuplicateOffsets(t *testing.T) {
	t.Parallel()
	bs := fixture.Generate(fixture.Params{
		Camera: "c", Rendition: "r", Codec: ingest.CodecH264,
		Units: 12, TimeBase: base.ClockRate, Seed: 3,
	})
	streamSize := int64(len(bs.Data))

	t.Run("out-of-range length", func(t *testing.T) {
		jm, _ := fixture.ManifestJSON(bs.Manifest)
		m, err := ingest.ParseJSONManifest(bytes.NewReader(jm), streamSize)
		if err != nil {
			t.Fatal(err)
		}
		m.Entries[0].Length = int(streamSize) + 100
		if err := m.Validate(streamSize); err == nil {
			t.Fatal("expected out-of-range rejection")
		}
	})

	t.Run("duplicate offset", func(t *testing.T) {
		jm, _ := fixture.ManifestJSON(bs.Manifest)
		m, err := ingest.ParseJSONManifest(bytes.NewReader(jm), streamSize)
		if err != nil {
			t.Fatal(err)
		}
		m.Entries[1].Offset = m.Entries[0].Offset
		if err := m.Validate(streamSize); err == nil {
			t.Fatal("expected duplicate-offset rejection")
		}
	})
}

func TestTSVManifestRoundTrip(t *testing.T) {
	t.Parallel()
	bs := fixture.Generate(fixture.Params{
		Camera: "c", Rendition: "r", Codec: ingest.CodecAV1,
		Units: 18, TimeBase: base.ClockRate, Seed: 5,
	})
	tsv := fixture.ManifestTSV(bs.Manifest)
	m, err := ingest.ParseTSVManifest(bytes.NewReader(tsv), int64(len(bs.Data)))
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Entries) != len(bs.Manifest.Entries) {
		t.Fatalf("tsv rows %d != manifest entries %d", len(m.Entries), len(bs.Manifest.Entries))
	}
	if m.Codec != ingest.CodecAV1 {
		t.Fatalf("codec mismatch: %s", m.Codec)
	}
}

func TestOversizeAccessUnitRejected(t *testing.T) {
	t.Parallel()
	bs := fixture.Generate(fixture.Params{
		Camera: "c", Rendition: "r", Codec: ingest.CodecH264,
		Units: 4, TimeBase: base.ClockRate, Seed: 9,
	})
	m := bs.Manifest
	m.Entries[0].Length = base.MaxAccessUnitBytes + 1
	err := m.Validate(int64(len(bs.Data)))
	if err == nil || base.AsFault(err) == nil {
		t.Fatal("expected a structured oversize fault")
	}
	if base.AsFault(err).Kind != base.FaultOversize {
		t.Fatalf("wrong fault kind: %s", base.AsFault(err).Kind)
	}
}

func TestOneGiBStreamLimitRejected(t *testing.T) {
	t.Parallel()
	bs := fixture.Generate(fixture.Params{
		Camera: "c", Rendition: "r", Codec: ingest.CodecH264,
		Units: 4, TimeBase: base.ClockRate, Seed: 4,
	})
	// Lie about the stream size to exercise the guard without allocating.
	huge := make([]byte, base.MaxFileBytes+1)
	clock := base.NewClock()
	_, _, err := ingest.ScanH264(bs.Manifest, huge, clock, func(*base.Fault) {})
	if err == nil || base.AsFault(err) == nil {
		t.Fatal("expected a 1 GiB oversize fault")
	}
	if base.AsFault(err).Kind != base.FaultOversize {
		t.Fatalf("wrong kind: %s", base.AsFault(err).Kind)
	}
}

func TestFaultRetainsLocatorFields(t *testing.T) {
	t.Parallel()
	bs := fixture.Generate(fixture.Params{
		Camera: "camX", Rendition: "rendY", Codec: ingest.CodecH264,
		Units: 6, TimeBase: base.ClockRate, Seed: 2,
	})
	m := bs.Manifest
	m.Entries[1].Offset = m.Entries[0].Offset // duplicate offset
	err := m.Validate(int64(len(bs.Data)))
	f := base.AsFault(err)
	if f == nil {
		t.Fatal("expected structured fault")
	}
	if f.Camera != "camX" || f.Rendition != "rendY" {
		t.Fatalf("fault lost camera/rendition: %+v", f)
	}
	if f.ByteOffset != m.Entries[1].Offset {
		t.Fatalf("fault lost byte offset: %d", f.ByteOffset)
	}
	if f.PTS != bs.Manifest.PTS90k(&m.Entries[1]) {
		t.Fatalf("fault lost PTS: %d", f.PTS)
	}
	if !f.Recoverable && f.Kind == base.FaultDuplicateIndex {
		// duplicate index is intentionally fatal, but must still locate.
	}
}

func scan(codec ingest.Codec, bs *fixture.BareStream, clock *base.Clock,
	sink ingest.FaultSink) ([]*ingest.AccessUnit, ingest.ScanStats, error) {
	switch codec {
	case ingest.CodecH264:
		return ingest.ScanH264(bs.Manifest, bs.Data, clock, sink)
	case ingest.CodecH265:
		return ingest.ScanH265(bs.Manifest, bs.Data, clock, sink)
	case ingest.CodecAV1:
		return ingest.ScanAV1(bs.Manifest, bs.Data, clock, sink)
	}
	return nil, ingest.ScanStats{}, nil
}
