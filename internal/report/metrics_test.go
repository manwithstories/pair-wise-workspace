package report

import (
	"strings"
	"testing"
)

func TestRatesAndRenderTokens(t *testing.T) {
	m := NewMetrics()
	m.Fixtures = 5
	m.RoundtripOK, m.RoundtripTotal = 8, 8
	m.KeyframeHits, m.KeyframeWanted = 99, 100
	m.StableSegments, m.TotalSegments = 6, 6
	m.Note("synthetic check")

	if m.RoundtripRate() != 100 {
		t.Fatalf("roundtrip rate = %v", m.RoundtripRate())
	}
	if m.KeyframeHitRate() != 99 {
		t.Fatalf("keyframe rate = %v", m.KeyframeHitRate())
	}
	if m.SegmentStability() != 100 {
		t.Fatalf("stability = %v", m.SegmentStability())
	}
	out := m.Render()
	for _, token := range []string{
		"PASS", "ROUNDTRIP=100%", "KEYFRAME_HIT=99%",
		"SEGMENT_HASH_STABLE=100%", "FIXTURES=5", "FIRST_BLOCKER=none",
	} {
		if !strings.Contains(out, token) {
			t.Fatalf("render missing %q:\n%s", token, out)
		}
	}
	if !m.Acceptable() {
		t.Fatal("thresholds should pass")
	}
}

func TestAcceptanceThresholds(t *testing.T) {
	base := func() *Metrics {
		m := NewMetrics()
		m.Fixtures = 4
		m.RoundtripOK, m.RoundtripTotal = 4, 4
		m.KeyframeHits, m.KeyframeWanted = 100, 100
		m.StableSegments, m.TotalSegments = 4, 4
		return m
	}
	if !base().Acceptable() {
		t.Fatal("base metrics must pass")
	}
	tooFewFixtures := base()
	tooFewFixtures.Fixtures = 3
	if tooFewFixtures.Acceptable() {
		t.Fatal("FIXTURES<4 must fail")
	}
	badRoundtrip := base()
	badRoundtrip.RoundtripOK = 3
	if badRoundtrip.Acceptable() {
		t.Fatal("ROUNDTRIP<100 must fail")
	}
	lowKeyframes := base()
	lowKeyframes.KeyframeHits = 98
	if lowKeyframes.Acceptable() {
		t.Fatal("KEYFRAME_HIT<99 must fail")
	}
	unstable := base()
	unstable.StableSegments = 3
	if unstable.Acceptable() {
		t.Fatal("unstable segments must fail")
	}
}

func TestZeroDenominatorSafe(t *testing.T) {
	m := NewMetrics()
	if m.RoundtripRate() != 0 || m.KeyframeHitRate() != 0 || m.SegmentStability() != 0 {
		t.Fatal("empty metrics must report zero rates, not divide-by-zero")
	}
	if m.FirstBlocker != "none" {
		t.Fatalf("default blocker = %q", m.FirstBlocker)
	}
}

func TestBlockerRecorded(t *testing.T) {
	m := NewMetrics()
	m.FirstBlocker = "audio-normalize"
	if !strings.Contains(m.Render(), "FIRST_BLOCKER=audio-normalize") {
		t.Fatal("blocker token missing from render")
	}
}
