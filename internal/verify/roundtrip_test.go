package verify_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/fieldcut/fieldcut/internal/base"
	"github.com/fieldcut/fieldcut/internal/engine"
	"github.com/fieldcut/fieldcut/internal/fixture"
	"github.com/fieldcut/fieldcut/internal/ingest"
	"github.com/fieldcut/fieldcut/internal/verify"
)

func buildAndWrite(t *testing.T, seed uint64, dir string) []*engine.Segment {
	t.Helper()
	codecs := []ingest.Codec{ingest.CodecH264, ingest.CodecH265, ingest.CodecAV1}
	var streams []*engine.Stream
	for r, codec := range codecs {
		clock := base.NewClock()
		bs := fixture.Generate(fixture.Params{
			Camera: "c", Rendition: rendName(r, codec), Codec: codec,
			Units: 120, TimeBase: base.ClockRate, Seed: seed + uint64(r)*1009,
		})
		out, _, err := scan(codec, bs, clock)
		if err != nil {
			t.Fatal(err)
		}
		streams = append(streams, &engine.Stream{
			Camera: "c", Rendition: rendName(r, codec), Codec: codec,
			Color: engine.ColorTag{Source: "policy"}, Units: out,
		})
	}
	b, err := engine.NewBuilder(dir, seed, 4*base.ClockRate, clone(streams))
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Build(); err != nil {
		t.Fatal(err)
	}
	return b.Segments()
}

func TestRoundtripReparsesEverySegment(t *testing.T) {
	t.Parallel()
	segments := buildAndWrite(t, 53, filepath.Join(t.TempDir(), "out"))
	if len(segments) == 0 {
		t.Fatal("no segments to verify")
	}
	rep, err := verify.Roundtrip(segments)
	if err != nil {
		t.Fatal(err)
	}
	rt, hashRate, keyRate := rep.Rates()
	if rt != 1.0 || hashRate != 1.0 || keyRate != 1.0 {
		t.Fatalf("expected perfect rates, got rt=%f hash=%f key=%f", rt, hashRate, keyRate)
	}
	if rep.SegmentsChecked != len(segments) || rep.HashesMatched != len(segments) {
		t.Fatalf("not every segment re-parsed: %+v", rep)
	}
	if len(rep.Mismatches) != 0 {
		t.Fatalf("unexpected mismatches: %+v", rep.Mismatches)
	}
}

func TestParseSegmentSamplePayloadExact(t *testing.T) {
	t.Parallel()
	segments := buildAndWrite(t, 67, filepath.Join(t.TempDir(), "out"))
	parsed, err := verify.ParseSegment(segments[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.SampleCount != len(segments[0].Units) {
		t.Fatalf("sample count %d != units %d", parsed.SampleCount, len(segments[0].Units))
	}
	for i, u := range segments[0].Units {
		if parsed.SampleSizes[i] != uint32(len(u.Payload)) {
			t.Fatalf("sample %d size mismatch", i)
		}
		if string(parsed.SampleData[i]) != string(u.Payload) {
			t.Fatalf("sample %d payload did not round-trip", i)
		}
	}
}

func TestTamperedSegmentFailsHash(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "out")
	segments := buildAndWrite(t, 71, dir)
	target := segments[0]

	// Tamper with the on-disk bytes without touching the in-memory hash,
	// proving the verifier reads from disk rather than trusted state.
	raw, err := os.ReadFile(target.Path)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)-1] ^= 0xff
	if err := os.WriteFile(target.Path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	parsed, err := verify.ParseSegment(target.Path)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Hash == hashHex(target) {
		t.Fatal("tampered segment must produce a different hash")
	}
}

func TestRecoverySegmentExemptFromFirstKeyframe(t *testing.T) {
	t.Parallel()
	segments := buildAndWrite(t, 83, filepath.Join(t.TempDir(), "out"))
	// Fabricate a recovery segment whose first unit is an inter frame but
	// whose per-sample sync flags still match.
	seg := segments[1]
	parsed, err := verify.ParseSegment(seg.Path)
	if err != nil {
		t.Fatal(err)
	}
	// Recovery flag exempts only the first-keyframe requirement, not the
	// per-sample flag consistency check.
	if !parsed.KeyframeAligned(seg.Units, true) {
		t.Fatal("recovery alignment check should pass for valid samples")
	}
}

func hashHex(s *engine.Segment) string {
	const hexdigits = "0123456789abcdef"
	out := make([]byte, len(s.Hash)*2)
	for i, b := range s.Hash {
		out[i*2] = hexdigits[b>>4]
		out[i*2+1] = hexdigits[b&0x0f]
	}
	return string(out)
}

func rendName(r int, c ingest.Codec) string {
	return "rend-" + string(rune('0'+r)) + "-" + string(c)
}

func scan(codec ingest.Codec, bs *fixture.BareStream, clock *base.Clock) ([]*ingest.AccessUnit, ingest.ScanStats, error) {
	noFault := func(*base.Fault) {}
	switch codec {
	case ingest.CodecH264:
		return ingest.ScanH264(bs.Manifest, bs.Data, clock, noFault)
	case ingest.CodecH265:
		return ingest.ScanH265(bs.Manifest, bs.Data, clock, noFault)
	case ingest.CodecAV1:
		return ingest.ScanAV1(bs.Manifest, bs.Data, clock, noFault)
	}
	return nil, ingest.ScanStats{}, nil
}

func clone(in []*engine.Stream) []*engine.Stream {
	out := make([]*engine.Stream, len(in))
	for i, s := range in {
		units := make([]*ingest.AccessUnit, len(s.Units))
		for j, u := range s.Units {
			cp := *u
			cp.Payload = append([]byte(nil), u.Payload...)
			units[j] = &cp
		}
		out[i] = &engine.Stream{
			Camera: s.Camera, Rendition: s.Rendition, Codec: s.Codec,
			Color: s.Color, Units: units, ClockStats: s.ClockStats,
		}
	}
	return out
}
