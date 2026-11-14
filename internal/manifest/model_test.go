package manifest

import (
	"errors"
	"testing"
)

func testManifest() *Manifest {
	return &Manifest{
		TargetUS:    4_000_000,
		ToleranceUS: 2_000,
		Template:    DefaultTemplate,
		Representations: []Representation{
			{ID: "r720", Bandwidth: 3_000_000, Segments: []Segment{{Number: 1, StartUS: 0, DurationUS: 4_000_000}}},
			{ID: "r1080", Bandwidth: 6_000_000, Segments: []Segment{{Number: 1, StartUS: 0, DurationUS: 4_000_000}}},
		},
	}
}

func TestValidateSortsAndDeduplicates(t *testing.T) {
	m := testManifest()
	m.Representations = append(m.Representations,
		Representation{ID: "r480a", Bandwidth: 1_500_000, Segments: []Segment{{Number: 1, DurationUS: 4_000_000}}},
		Representation{ID: "r480b", Bandwidth: 1_500_000, Segments: []Segment{{Number: 1, DurationUS: 4_000_000}}},
	)
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(m.Representations) != 3 {
		t.Fatalf("got %d representations after bandwidth dedupe", len(m.Representations))
	}
	got := []string{m.Representations[0].ID, m.Representations[1].ID, m.Representations[2].ID}
	want := []string{"r480a", "r720", "r1080"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order[%d]=%q want %q", i, got[i], want[i])
		}
	}
}

func TestDuplicateRepresentationIDIsError(t *testing.T) {
	m := testManifest()
	m.Representations[1].ID = m.Representations[0].ID
	err := m.Validate()
	if err == nil {
		t.Fatal("expected duplicate ID error")
	}
	if code := errorCode(err); code != "manifest.duplicate_representation" {
		t.Fatalf("code=%q", code)
	}
}

func TestRollWindowKeepsNumbers(t *testing.T) {
	m := testManifest()
	rep := &m.Representations[0]
	rep.Segments = nil
	for i := uint64(1); i <= 5; i++ {
		rep.Segments = append(rep.Segments, Segment{Number: i, StartUS: int64(i-1) * 1_000_000, DurationUS: 1_000_000})
	}
	m.Representations = m.Representations[:1]
	m.TargetUS, m.ToleranceUS = 1_000_000, 2_000
	m.Window.Version = 1
	m.Discontinuities = []Discontinuity{{RepresentationID: "r720", SegmentNumber: 1}, {RepresentationID: "r720", SegmentNumber: 5}}
	m.ResyncPoints = []ResyncPoint{{RepresentationID: "r720", SegmentNumber: 1}}
	m.Stats = []RepresentationStats{{RepresentationID: "r720"}}
	if err := RollWindow(m, 2); err != nil {
		t.Fatal(err)
	}
	if m.Window.Version != 2 || m.Window.StartNumber != 4 || m.Window.Size != 2 {
		t.Fatalf("window=%+v", m.Window)
	}
	if len(m.Representations[0].Segments) != 2 || m.Representations[0].Segments[0].Number != 4 {
		t.Fatalf("segments=%+v", m.Representations[0].Segments)
	}
	if len(m.Discontinuities) != 1 || m.Discontinuities[0].SegmentNumber != 5 || len(m.ResyncPoints) != 0 || len(m.Stats) != 0 {
		t.Fatalf("stale window metadata: disc=%+v resync=%+v stats=%+v", m.Discontinuities, m.ResyncPoints, m.Stats)
	}
}

func TestMergeRepresentationsVersionRules(t *testing.T) {
	old := Representation{ID: "r1080", Bandwidth: 6_000_000, Segments: []Segment{{Number: 1, DurationUS: 4_000_000}}}
	newer := old
	newer.Segments = []Segment{{Number: 1, DurationUS: 4_100_000}}
	merged, err := MergeRepresentations(old, newer, 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	if merged.Segments[0].DurationUS != 4_100_000 {
		t.Fatalf("newer segment did not win: %+v", merged.Segments[0])
	}
	if _, err := MergeRepresentations(old, newer, 2, 2); errorCode(err) != "merge.content_conflict" {
		t.Fatalf("expected content conflict, got %v", err)
	}
}

func errorCode(err error) string {
	var target *Error
	if errors.As(err, &target) {
		return target.Code
	}
	return ""
}

func TestTemplateExpandsAllMacros(t *testing.T) {
	template, err := ParseTemplate("seg/$RepresentationID$/$Bandwidth$/$Number%05d$/$Time$/$TimeMS%06d$.m4s")
	if err != nil {
		t.Fatal(err)
	}
	got, err := template.Expand("r1080", 6_000_000, 7, 4_250_000)
	if err != nil {
		t.Fatal(err)
	}
	want := "seg/r1080/6000000/00007/4.250/004250.m4s"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestTemplateRejectsUnknownMacroAndZeroWidth(t *testing.T) {
	if _, err := ParseTemplate("seg/$Unknown$.m4s"); errorCode(err) != "template.unknown_macro" {
		t.Fatalf("unknown macro error=%v", err)
	}
	if _, err := ParseTemplate("seg/$Number%0d$.m4s"); errorCode(err) != "template.zero_width" {
		t.Fatalf("zero width error=%v", err)
	}
}

func TestTemplateTimeMillisecondsWidth(t *testing.T) {
	template, err := ParseTemplate("$Time%05d$")
	if err != nil {
		t.Fatal(err)
	}
	got, err := template.Expand("r", 1, 1, 4_250_000)
	if err != nil {
		t.Fatal(err)
	}
	if got != "04250" {
		t.Fatalf("got %q", got)
	}
}

func TestValidateChecksTemplateSyntax(t *testing.T) {
	m := testManifest()
	m.Template = "seg/$Unknown$.m4s"
	if errorCode(m.Validate()) != "template.unknown_macro" {
		t.Fatal("manifest validation accepted an invalid template")
	}
}

func TestExpandAllRejectsDuplicatePaths(t *testing.T) {
	m := testManifest()
	m.Template = "seg/$Number%05d$.m4s"
	m.Representations[0].Segments = []Segment{{Number: 1, DurationUS: 4_000_000}}
	m.Representations[1].Segments = []Segment{{Number: 1, DurationUS: 4_000_000}}
	_, err := ExpandAll(m)
	if errorCode(err) != "template.duplicate_path" {
		t.Fatalf("expected duplicate path error, got %v", err)
	}
}
