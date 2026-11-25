package engine_test

import (
	"path/filepath"
	"testing"

	"github.com/pion/rtp"

	"github.com/fieldcut/fieldcut/internal/base"
	"github.com/fieldcut/fieldcut/internal/engine"
	"github.com/fieldcut/fieldcut/internal/fixture"
	"github.com/fieldcut/fieldcut/internal/ingest"
)

func buildStreams(t *testing.T, seed uint64) []*engine.Stream {
	t.Helper()
	codecs := []ingest.Codec{ingest.CodecH264, ingest.CodecH265, ingest.CodecAV1}
	var streams []*engine.Stream
	for r, codec := range codecs {
		clock := base.NewClock()
		bs := fixture.Generate(fixture.Params{
			Camera: "c", Rendition: name(r, codec), Codec: codec,
			Units: 120, TimeBase: base.ClockRate, Seed: seed + uint64(r)*1009,
		})
		out, _, err := scan(codec, bs, clock)
		if err != nil {
			t.Fatal(err)
		}
		streams = append(streams, &engine.Stream{
			Camera: "c", Rendition: name(r, codec), Codec: codec,
			Color: engine.ColorTag{Primaries: 9, Transfer: 16, Matrix: 9, Source: "policy"},
			Units: out,
		})
	}
	return streams
}

func TestFanOutBoundariesAreCommonKeyframes(t *testing.T) {
	t.Parallel()
	streams := buildStreams(t, 17)
	plan := engine.FanOut(streams)
	if len(plan.Boundaries) < 2 {
		t.Fatal("expected multiple shared boundaries")
	}
	for _, s := range streams {
		ranges := plan.Ranges[s.Rendition]
		if len(ranges) != len(plan.Boundaries) {
			t.Fatalf("rendition %s ranges %d != boundaries %d", s.Rendition, len(ranges), len(plan.Boundaries))
		}
		// Every non-empty segment must start on a keyframe.
		for bi, rng := range ranges {
			if rng[0] == rng[1] {
				continue
			}
			if !s.Units[rng[0]].Keyframe {
				t.Fatalf("rendition %s boundary %d not on a keyframe", s.Rendition, bi)
			}
		}
	}
}

func TestRTPReorderAndLoss(t *testing.T) {
	t.Parallel()
	probe := engine.NewRTPProbe(12345, 8)
	loc := base.Locator{Camera: "c", Rendition: "r", PTS: 0}

	mk := func(seq uint16) *rtp.Packet {
		return &rtp.Packet{Header: rtp.Header{
			Version: 2, SSRC: 12345, SequenceNumber: seq, Timestamp: 90000,
		}, Payload: []byte{byte(seq)}}
	}
	// Out of order: 0 (emitted immediately), then 2 buffered, then 1 fills
	// the hole and the contiguous run 1..2 flushes in order.
	if _, f := probe.Push(mk(0), loc); len(f) != 0 {
		t.Fatalf("seq 0 should be clean, got %d faults", len(f))
	}
	if buffered, _ := probe.Push(mk(2), loc); len(buffered) != 0 {
		t.Fatalf("seq 2 ahead of expectation must buffer, not emit")
	}
	out, _ := probe.Push(mk(1), loc)
	if len(out) != 2 || out[0].SequenceNumber != 1 || out[1].SequenceNumber != 2 {
		t.Fatalf("expected ordered flush [1 2], got %v", out)
	}
	if probe.Reordered == 0 {
		t.Fatal("expected a reorder event")
	}

	// A jump beyond the jitter window is a loss with rendition + PTS locator.
	_, faults := probe.Push(mk(20), loc)
	if len(faults) == 0 || faults[0].Rendition != "r" || faults[0].PTS != 0 {
		t.Fatalf("loss fault must carry rendition and clock position: %+v", faults)
	}
	if probe.Losses == 0 {
		t.Fatal("expected loss accounting")
	}
}

func TestRTPWrongSSRCRejected(t *testing.T) {
	t.Parallel()
	probe := engine.NewRTPProbe(1, 4)
	pkt := &rtp.Packet{Header: rtp.Header{Version: 2, SSRC: 2, SequenceNumber: 0}}
	_, faults := probe.Push(pkt, base.Locator{})
	if len(faults) == 0 || faults[0].Kind != base.FaultRTPLoss {
		t.Fatalf("wrong SSRC must raise a recoverable RTP fault: %+v", faults)
	}
}

func TestBuilderDeterministicHashesAcrossRuns(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	streams := buildStreams(t, 31)
	ticks := uint64(4) * base.ClockRate

	b1, err := engine.NewBuilder(filepath.Join(dir, "a"), 31, ticks, clone(streams))
	if err != nil {
		t.Fatal(err)
	}
	if err := b1.Build(); err != nil {
		t.Fatal(err)
	}
	b2, err := engine.NewBuilder(filepath.Join(dir, "b"), 31, ticks, clone(streams))
	if err != nil {
		t.Fatal(err)
	}
	if err := b2.Build(); err != nil {
		t.Fatal(err)
	}
	if len(b1.Segments()) == 0 {
		t.Fatal("no segments produced")
	}
	if len(b1.Segments()) != len(b2.Segments()) {
		t.Fatalf("segment count differs: %d vs %d", len(b1.Segments()), len(b2.Segments()))
	}
	for i := range b1.Segments() {
		if fmtHash(b1.Segments()[i]) != fmtHash(b2.Segments()[i]) {
			t.Fatalf("segment %d hash differs across same-seed runs", i)
		}
	}
}

func TestCheckpointResumeSkipsCommitted(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "resume")
	streams := buildStreams(t, 41)
	ticks := uint64(4) * base.ClockRate

	b1, err := engine.NewBuilder(dir, 41, ticks, clone(streams))
	if err != nil {
		t.Fatal(err)
	}
	if err := b1.Build(); err != nil {
		t.Fatal(err)
	}
	firstCount := len(b1.Segments())

	// Reopen the same directory: checkpoint makes the rebuild a no-op.
	b2, err := engine.NewBuilder(dir, 41, ticks, clone(streams))
	if err != nil {
		t.Fatal(err)
	}
	if err := b2.Build(); err != nil {
		t.Fatal(err)
	}
	if len(b2.Segments()) != 0 {
		t.Fatalf("resumed build should skip all %d committed segments", firstCount)
	}
}

func TestColorResolutionPriority(t *testing.T) {
	t.Parallel()
	tags := []engine.ColorTag{
		{Primaries: 6, Transfer: 6, Matrix: 6, Source: "bitstream"},
		{Primaries: 1, Transfer: 1, Matrix: 1, Source: "manifest"},
		{Primaries: 9, Transfer: 16, Matrix: 9, Source: "policy"},
	}
	winner, _ := engine.ResolveColor(tags)
	if winner.Source != "policy" || winner.Primaries != 9 {
		t.Fatalf("policy source must win, got %+v", winner)
	}
}

func name(r int, c ingest.Codec) string { return "rend-" + string(rune('0'+r)) + "-" + string(c) }

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

func fmtHash(s *engine.Segment) string {
	return string(append([]byte(nil), s.Hash...))
}
