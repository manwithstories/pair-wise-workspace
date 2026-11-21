package clock

import (
	"testing"
)

func TestMapSamplesExplicitCases(t *testing.T) {
	// 1ms ticks, 90kHz-style awkward timescale is covered separately.
	tb := MustTimebase(DefaultTimestampScale)
	samples := []Sample{
		{Index: 0, DTS: 40, PTS: 40}, // starts at 40ms: timestamps are rebased
		{Index: 1, DTS: 40, PTS: 35}, // duplicate DTS with regressing PTS
		{Index: 2, DTS: 80, PTS: 80},
		{Index: 3, DTS: 120, PTS: 120},
	}
	m, err := MapSamples(tb, DefaultTimescale, samples, 40_000_000)
	if err != nil {
		t.Fatal(err)
	}
	if m.Shift != -40_000 {
		t.Fatalf("shift=%d", m.Shift)
	}
	if m.Stats.DuplicateDTS != 1 || m.Stats.PTSRegression != 1 || m.Stats.NegativeOffsets != 1 {
		t.Fatalf("stats=%+v", m.Stats)
	}
	if m.Samples[0].DTS != 0 || m.Samples[1].DTS != 0 {
		t.Fatalf("duplicate DTS must become zero-based, got %+v", m.Samples)
	}
	if m.Samples[0].Duration != 40_000 {
		t.Fatalf("duplicate DTS duration should fall back to default, got %d", m.Samples[0].Duration)
	}
	if m.Samples[1].Offset != 35_000-40_000 {
		t.Fatalf("negative offset lost: %d", m.Samples[1].Offset)
	}
}

func TestMapSamplesNegativeOrigin(t *testing.T) {
	tb := MustTimebase(DefaultTimestampScale)
	samples := []Sample{
		{Index: 0, DTS: -20, PTS: 0},
		{Index: 1, DTS: 10, PTS: 10},
	}
	m, err := MapSamples(tb, DefaultTimescale, samples, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !m.Stats.NegativeStart || m.Origin != -20_000 {
		t.Fatalf("negative start not recorded: %+v", m.Stats)
	}
	if m.Samples[0].DTS != 0 {
		t.Fatalf("shift failed: %d", m.Samples[0].DTS)
	}
}

func TestMapSamplesRounding(t *testing.T) {
	// One tick is 100ns; at a 30kHz timescale 100ns is 0.003 ticks, which must
	// round rather than truncate silently.
	tb := MustTimebase(100)
	samples := []Sample{{Index: 0, DTS: 0, PTS: 0}, {Index: 1, DTS: 100, PTS: 100}}
	m, err := MapSamples(tb, 30_000, samples, 0)
	if err != nil {
		t.Fatal(err)
	}
	if m.Stats.Rounded == 0 {
		t.Fatalf("expected rounded statistics")
	}
	if m.Samples[1].DTS != 0 && m.Samples[1].DTS != 1 {
		t.Fatalf("unexpected rounding: %d", m.Samples[1].DTS)
	}
}

func TestDeterministicDigest(t *testing.T) {
	tb := MustTimebase(DefaultTimestampScale)
	samples := []Sample{
		{Index: 2, DTS: 30, PTS: 30},
		{Index: 0, DTS: 10, PTS: 10},
		{Index: 1, DTS: 20, PTS: 25},
	}
	a, err := MapSamples(tb, DefaultTimescale, samples, 10_000_000)
	if err != nil {
		t.Fatal(err)
	}
	b, err := MapSamples(tb, DefaultTimescale, samples, 10_000_000)
	if err != nil {
		t.Fatal(err)
	}
	if a.Digest() != b.Digest() {
		t.Fatalf("digest not deterministic: %s vs %s", a.Digest(), b.Digest())
	}
	if a.SourceOrder[0] != 0 || a.SourceOrder[2] != 2 {
		t.Fatalf("source order not tracked: %v", a.SourceOrder)
	}
	if a.Duration() != 30_000 {
		t.Fatalf("duration=%d", a.Duration())
	}
}
