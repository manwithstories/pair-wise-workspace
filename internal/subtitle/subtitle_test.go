package subtitle

import (
	"strings"
	"testing"

	"avrepair/internal/clock"
)

const sampleSRT = `1
00:00:01,000 --> 00:00:03,000
first cue

2
00:00:03,500 --> 00:00:05,000
second cue
second line
`

const sampleVTT = `WEBVTT

00:00:01.000 --> 00:00:03.000
hello

00:00:03.500 --> 00:00:05.000
world
`

func TestParseSRT(t *testing.T) {
	tr, err := Parse("sub", FormatSRT, strings.NewReader(sampleSRT))
	if err != nil {
		t.Fatal(err)
	}
	if len(tr.Cues) != 2 {
		t.Fatalf("cues=%d want 2", len(tr.Cues))
	}
	if tr.Cues[0].StartNS != clock.Second || tr.Cues[0].EndNS != 3*clock.Second {
		t.Errorf("cue0 bounds wrong: %+v", tr.Cues[0])
	}
	if tr.Cues[1].Text != "second cue\nsecond line" {
		t.Errorf("multiline text wrong: %q", tr.Cues[1].Text)
	}
}

func TestParseWebVTT(t *testing.T) {
	tr, err := Parse("sub", FormatWebVTT, strings.NewReader(sampleVTT))
	if err != nil {
		t.Fatal(err)
	}
	if len(tr.Cues) != 2 || tr.Cues[0].Text != "hello" {
		t.Fatalf("vtt parse wrong: %+v", tr.Cues)
	}
}

func TestParseErrors(t *testing.T) {
	if _, err := Parse("x", Format("bogus"), strings.NewReader("")); err == nil ||
		!clock.HasCode(err, clock.CodeSubtitleParse) {
		t.Fatalf("bad format must map to CodeSubtitleParse, got %v", err)
	}
	// Truncated SRT with an unclosed timestamp range.
	bad := "1\n00:00:01,000 -->\ntext\n"
	if _, err := Parse("x", FormatSRT, strings.NewReader(bad)); err == nil {
		t.Fatal("malformed SRT must fail")
	}
}

func TestReconcileOverlap(t *testing.T) {
	tr := &Track{
		Name:   "sub",
		Format: FormatSRT,
		Cues: []Cue{
			{Index: 0, StartNS: 1_000_000, EndNS: 5_000_000, Text: "a"},
			{Index: 1, StartNS: 3_000_000, EndNS: 7_000_000, Text: "b"},
		},
	}
	res := Reconcile(tr)
	if len(res.Cues) != 2 {
		t.Fatalf("cues=%d want 2", len(res.Cues))
	}
	if res.Cues[0].EndNS != res.Cues[1].StartNS-1 {
		t.Errorf("overlap not trimmed: %+v", res.Cues[0])
	}
	if OverlapCount(res.Errors) != 1 {
		t.Errorf("overlap errors=%v", res.Errors)
	}
}

func TestReconcileRollbackAndIsolation(t *testing.T) {
	tr := &Track{
		Name: "sub",
		Cues: []Cue{
			{Index: 0, StartNS: 10_000_000, EndNS: 10_500_000},
			{Index: 1, StartNS: 11_000_000, EndNS: 11_500_000}, // healthy gap
			{Index: 2, StartNS: 1_000_000, EndNS: 2_000_000},   // >500ms rollback -> isolated
		},
	}
	res := Reconcile(tr)
	if len(res.Isolated) != 1 || res.Isolated[0].Index != 2 {
		t.Fatalf("isolated=%v", res.Isolated)
	}
	starts := make([]int64, len(res.Cues))
	for i, c := range res.Cues {
		starts[i] = c.StartNS
	}
	if err := clock.CheckMonotonic("sub", starts); err != nil {
		t.Fatal(err)
	}
}

func TestReconcileSmallRollbackRepaired(t *testing.T) {
	tr := &Track{
		Name: "sub",
		Cues: []Cue{
			{Index: 0, StartNS: 1_000_000, EndNS: 2_000_000},
			{Index: 1, StartNS: 5_000_000, EndNS: 5_500_000},
			// Tiny rollback; nudged to 5_500_001, just past cue1's end.
			{Index: 2, StartNS: 5_499_900, EndNS: 8_000_000},
		},
	}
	res := Reconcile(tr)
	if len(res.Isolated) != 0 {
		t.Fatalf("small rollback must not isolate: %v", res.Isolated)
	}
	if res.Repairs == 0 {
		t.Fatal("rollback should be repaired")
	}
	starts := make([]int64, len(res.Cues))
	for i, c := range res.Cues {
		starts[i] = c.StartNS
	}
	if err := clock.CheckMonotonic("sub", starts); err != nil {
		t.Fatal(err)
	}
}

func TestReconcileDegenerateCueIsolated(t *testing.T) {
	tr := &Track{
		Name: "sub",
		Cues: []Cue{
			{Index: 0, StartNS: 1_000, EndNS: 1_002},
			{Index: 1, StartNS: 1_000, EndNS: 9_000}, // duplicate start nudged; cue0 collapses
		},
	}
	res := Reconcile(tr)
	if len(res.Cues) != 1 || len(res.Isolated) != 1 {
		t.Fatalf("want degenerate cue isolated, got cues=%d isolated=%d",
			len(res.Cues), len(res.Isolated))
	}
}
