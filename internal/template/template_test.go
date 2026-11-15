package template

import (
	"strings"
	"testing"

	"dashkit/internal/model"
)

func TestExpandPlaceholders(t *testing.T) {
	v := Values{Number: 7, Time: 180180, RepresentationID: "v720p", Bandwidth: 2500000}
	got, err := Expand("$RepresentationID$/seg_$Number%05d$_$Time$_$Bandwidth$_$$.m4s", v)
	if err != nil {
		t.Fatal(err)
	}
	want := "v720p/seg_00007_180180_2500000_$.m4s"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestExpandUnknownAndMalformed(t *testing.T) {
	if _, err := Expand("$Foo$", Values{}); err == nil || !strings.Contains(err.Error(), model.CodeUnknownHolder) {
		t.Fatalf("unknown placeholder: %v", err)
	}
	if _, err := Expand("seg_$Number.m4s", Values{}); err == nil {
		t.Fatal("malformed placeholder accepted")
	}
}

func TestExpandSegmentsURLs(t *testing.T) {
	u64 := func(v uint64) *uint64 { return &v }
	rep := &model.Representation{ID: "a64k", Bandwidth: 64000, Template: &model.SegmentTemplate{
		Timescale: 90000, StartNumber: 1, Media: "$RepresentationID$/$Number%03d$.m4s",
		Timeline: &model.SegmentTimeline{S: []*model.S{{T: u64(0), D: 100, R: 2}}},
	}}
	got, expanded, probs := ExpandSegments(rep)
	if len(probs) != 0 || len(got) != 3 || len(expanded) != 3 {
		t.Fatalf("got %v %d %d", probs, len(got), len(expanded))
	}
	if got[0] != "a64k/001.m4s" || got[2] != "a64k/003.m4s" {
		t.Fatalf("urls: %v", got)
	}
}
