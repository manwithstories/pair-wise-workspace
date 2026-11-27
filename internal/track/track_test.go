package track

import (
	"testing"

	"avrepair/internal/clock"
)

func TestTopoOrder(t *testing.T) {
	g, err := NewGraph([]*Node{
		{ID: "mux", Kind: KindSub, DependsOn: []string{"audio", "video"}},
		{ID: "audio", Kind: KindAudio, DependsOn: []string{"clock"}},
		{ID: "video", Kind: KindVideo, DependsOn: []string{"clock"}},
		{ID: "clock", Kind: KindSub},
	})
	if err != nil {
		t.Fatal(err)
	}
	order, err := g.Topo()
	if err != nil {
		t.Fatal(err)
	}
	pos := map[string]int{}
	for i, id := range order {
		pos[id] = i
	}
	if pos["clock"] >= pos["audio"] || pos["clock"] >= pos["video"] {
		t.Errorf("clock must precede a/v: %v", order)
	}
	if pos["audio"] >= pos["mux"] || pos["video"] >= pos["mux"] {
		t.Errorf("a/v must precede mux: %v", order)
	}
}

func TestTopoCycleAndBadRef(t *testing.T) {
	if _, err := NewGraph([]*Node{
		{ID: "a", DependsOn: []string{"missing"}},
	}); err == nil || !clock.HasCode(err, clock.CodeGraphCycle) {
		t.Fatalf("missing dependency must error, got %v", err)
	}
	g, err := NewGraph([]*Node{
		{ID: "a", DependsOn: []string{"b"}},
		{ID: "b", DependsOn: []string{"a"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.Topo(); err == nil || !clock.HasCode(err, clock.CodeGraphCycle) {
		t.Fatalf("cycle must yield CodeGraphCycle, got %v", err)
	}
}

func TestNormalizeAudio(t *testing.T) {
	g, _ := NewGraph([]*Node{
		{ID: "good", Kind: KindAudio, Layout: "stereo", Channels: 2, SampleRate: 44100},
		{ID: "weird", Kind: KindAudio, Layout: "quadraphonic-mystery", Channels: 4, SampleRate: 48000},
		{ID: "mismatch", Kind: KindAudio, Layout: "5.1", Channels: 2, SampleRate: 48000},
	})
	order, _ := g.Topo()
	nz, _ := Normalize(g, order)
	byID := map[string]*Normalized{}
	for _, n := range nz {
		byID[n.ID] = n
	}
	good := byID["good"]
	if good.SampleRate != TargetSampleRate || !good.Resampled {
		t.Errorf("resample missing: %+v", good)
	}
	weird := byID["weird"]
	if !weird.Isolated || !hasCode(weird.Errors, clock.CodeUnknownLayout) {
		t.Errorf("unknown layout must isolate: %+v", weird)
	}
	if byID["mismatch"].Channels != 6 {
		t.Errorf("declared layout must win: %+v", byID["mismatch"])
	}
}

func hasCode(errs []*clock.TrackError, code clock.ErrorCode) bool {
	for _, e := range errs {
		if e.Code == code {
			return true
		}
	}
	return false
}

func TestNormalizeColorConflict(t *testing.T) {
	g, _ := NewGraph([]*Node{
		{ID: "base", Kind: KindVideo, Color: ColorMeta{Source: "cam-a", Space: "bt709"}},
		{ID: "graded", Kind: KindVideo, DependsOn: []string{"base"},
			Color: ColorMeta{Source: "cam-b", Space: "bt709"}},
		{ID: "same", Kind: KindVideo, DependsOn: []string{"base"},
			Color: ColorMeta{Source: "cam-a", Space: "bt709"}},
	})
	order, _ := g.Topo()
	nz, _ := Normalize(g, order)
	byID := map[string]*Normalized{}
	for _, n := range nz {
		byID[n.ID] = n
	}
	if !byID["graded"].ColorConflict {
		t.Error("different color source must flag conflict")
	}
	// Source label is preserved, never overwritten.
	if byID["graded"].Color.Source != "cam-b" {
		t.Error("color source label must be preserved")
	}
	if byID["same"].ColorConflict {
		t.Error("identical color metadata must not conflict")
	}
}

func TestNormalizeIsTopologicalAndInputUntouched(t *testing.T) {
	base := &Node{ID: "base", Kind: KindVideo, Color: ColorMeta{Source: "x", Space: "bt709"}}
	child := &Node{ID: "child", Kind: KindVideo, DependsOn: []string{"base"},
		Color: ColorMeta{Source: "y", Space: "bt2020"}}
	g, _ := NewGraph([]*Node{child, base})
	order, _ := g.Topo()
	if order[0] != "base" {
		t.Fatalf("base must normalize first, got %v", order)
	}
	if _, errs := Normalize(g, order); len(errs) == 0 {
		t.Fatal("expected color conflict errors")
	}
	if base.Color.Source != "x" {
		t.Fatal("normalization must not mutate graph input")
	}
}
