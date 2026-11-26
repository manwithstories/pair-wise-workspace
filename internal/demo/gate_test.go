package demo

import (
	"testing"

	"tsproof/internal/segment"
)

func baselineMetrics() Metrics {
	return Metrics{
		RoundtripPct: 100.0, Segments: SegmentsTarget, HashMatch: "24/24",
		ReplayIdentical: "yes", CRCReject: 1, CCGaps: 1, Truncated: 1,
		PSISwitches: 1, ResumeSegments: 12,
	}
}

func TestPassesGateDetectsInconsistency(t *testing.T) {
	good := baselineMetrics()
	seek := segment.SeekReport{Requests: 32, Hits: 32}
	if !passes(good, seek) {
		t.Fatal("baseline metrics must pass the gate")
	}
	t.Run("roundtrip", func(t *testing.T) {
		m := baselineMetrics()
		m.RoundtripPct = 96.8
		if passes(m, seek) {
			t.Fatal("must fail")
		}
	})
	t.Run("segments", func(t *testing.T) {
		m := baselineMetrics()
		m.Segments = 23
		if passes(m, seek) {
			t.Fatal("must fail")
		}
	})
	t.Run("hash", func(t *testing.T) {
		m := baselineMetrics()
		m.HashMatch = "23/24"
		if passes(m, seek) {
			t.Fatal("must fail")
		}
	})
	t.Run("replay", func(t *testing.T) {
		m := baselineMetrics()
		m.ReplayIdentical = "no"
		if passes(m, seek) {
			t.Fatal("must fail")
		}
	})
	t.Run("damage-coverage", func(t *testing.T) {
		for _, mut := range []func(*Metrics){
			func(m *Metrics) { m.CRCReject = 0 },
			func(m *Metrics) { m.CCGaps = 0 },
			func(m *Metrics) { m.Truncated = 0 },
			func(m *Metrics) { m.PSISwitches = 0 },
		} {
			m := baselineMetrics()
			mut(&m)
			if passes(m, seek) {
				t.Fatal("missing damage signal must fail the gate")
			}
		}
	})
	t.Run("keyframe-miss", func(t *testing.T) {
		badSeek := segment.SeekReport{Requests: 32, Hits: 31}
		if passes(good, badSeek) {
			t.Fatal("keyframe miss must fail the gate")
		}
	})
}
