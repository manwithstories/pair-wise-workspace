package metrics

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/manwithstories/pair-wise-workspace/internal/forwarder"
	rtpx "github.com/manwithstories/pair-wise-workspace/internal/rtp"
	"github.com/manwithstories/pair-wise-workspace/internal/signaling"
)

func sample() Inputs {
	return Inputs{
		LocalAddr:     "127.0.0.1:5000",
		RemoteAddr:    "127.0.0.1:5001",
		Candidates:    []string{"sender:127.0.0.1:5000", "receiver:127.0.0.1:5001"},
		Loopback:      true,
		SourcePackets: 2000,
		StreamStats: rtpx.Summary{
			Received:      2012,
			Unique:        1979,
			Lost:          21,
			Duplicates:    33,
			Reordered:     89,
			JitterAvgMs:   2,
			JitterMaxMs:   4,
			ClockRate:     90000,
			ReorderWindow: 32,
			Streams: []rtpx.StreamStats{{
				SSRC: 0xC0FFEE01, WrapCount: 1, Received: 2012,
			}},
		},
		Forwarder: forwarder.Stats{Forwarded: 1979, Backpressure: 0},
		Signaling: signaling.Stats{MessagesForwarded: 6, SessionsCreated: 1},
		Duration:  2 * time.Second,
	}
}

func TestBuildCopiesCountersVerbatim(t *testing.T) {
	in := sample()
	r := Build(in)
	if r.ForwardedPackets != 1979 || r.LostPackets != 21 || r.DuplicatePackets != 33 {
		t.Fatalf("counters were not copied: %+v", r)
	}
	if r.CandidatePairs != 1 {
		t.Fatalf("candidate_pairs=%d, want 1", r.CandidatePairs)
	}
	if r.SSRC != 0xC0FFEE01 || r.WrapCount != 1 {
		t.Fatalf("SSRC metadata lost: %+v", r)
	}
	if r.DurationMs != 2000 {
		t.Fatalf("duration=%d, want 2000", r.DurationMs)
	}
}

func TestNoPairWithoutAddresses(t *testing.T) {
	in := sample()
	in.LocalAddr, in.RemoteAddr = "", ""
	if r := Build(in); r.CandidatePairs != 0 {
		t.Fatalf("candidate_pairs=%d with no addresses, want 0", r.CandidatePairs)
	}
}

func TestJSONFieldNamesAreStable(t *testing.T) {
	var buf bytes.Buffer
	if err := Build(sample()).Encode(&buf); err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatalf("output is not JSON: %v", err)
	}
	want := []string{
		"candidate_pairs", "loopback_only", "source_packets",
		"forwarded_packets", "backpressure_drops", "lost_packets",
		"duplicate_packets", "reordered_packets", "late_outside_window",
		"received_packets", "unique_packets", "jitter_avg_ms",
		"jitter_max_ms", "clock_rate", "reorder_window", "ssrc",
		"wrap_count", "signaling_messages", "signaling_rejected",
		"signaling_sessions", "duration_ms", "exit_code",
	}
	for _, key := range want {
		if _, ok := m[key]; !ok {
			t.Fatalf("missing JSON field %q", key)
		}
	}
}

func TestHumanSummaryLeadsWithAcceptanceFields(t *testing.T) {
	text := Build(sample()).Human()
	for _, want := range []string{
		"candidate_pairs=1", "forwarded_packets=1979", "lost_packets=21",
		"duplicate_packets=33", "loopback_only=true", "exit_code=0",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("human summary missing %q:\n%s", want, text)
		}
	}
}

func TestCheckAcceptsGoodReport(t *testing.T) {
	r := Build(sample())
	if problems := r.Check(DefaultThresholds(2000, 0.01, 0.02)); len(problems) != 0 {
		t.Fatalf("unexpected problems: %v", problems)
	}
}

func TestCheckCatchesEachViolation(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Report)
	}{
		{"no pair", func(r *Report) { r.CandidatePairs = 0 }},
		{"not loopback", func(r *Report) { r.LoopbackOnly = false }},
		{"too few forwarded", func(r *Report) { r.ForwardedPackets = 10 }},
		{"too many lost", func(r *Report) { r.LostPackets = 900 }},
		{"too few duplicates", func(r *Report) { r.DuplicatePackets = 0 }},
		{"jitter over budget", func(r *Report) { r.JitterAvgMs = 50 }},
		{"max below average", func(r *Report) { r.JitterMaxMs = 1 }},
	}
	th := DefaultThresholds(2000, 0.01, 0.02)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := Build(sample())
			tc.mutate(&r)
			if problems := r.Check(th); len(problems) == 0 {
				t.Fatalf("violation %q was not detected: %+v", tc.name, r)
			}
		})
	}
}

func TestDefaultThresholdsTrackRates(t *testing.T) {
	densely := DefaultThresholds(2000, 0.01, 0.02)
	if densely.MinForwarded != 1940 {
		t.Fatalf("MinForwarded=%d, want 1940 (97%% of 2000)", densely.MinForwarded)
	}
	if densely.MinLost != 10 || densely.MaxLost != 42 {
		t.Fatalf("loss bounds are %d..%d, want 10..42", densely.MinLost, densely.MaxLost)
	}
	// A zero-loss run must be allowed to report zero loss.
	clean := DefaultThresholds(100, 0, 0)
	if clean.MinLost != 0 || clean.MinDup != 0 {
		t.Fatalf("zero rates must yield zero floors, got lost>=%d dup>=%d", clean.MinLost, clean.MinDup)
	}
}
