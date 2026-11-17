package validate_test

import (
	"strings"
	"testing"

	"hdrcheck/internal/fixture"
	"hdrcheck/internal/garc"
	"hdrcheck/internal/hdr"
	"hdrcheck/internal/validate"
)

func buildReport(t *testing.T, name string) *validate.Report {
	t.Helper()
	for _, sc := range fixture.Scenarios() {
		if sc.Name != name {
			continue
		}
		b, _ := sc.Build()
		data, _ := b.Build()
		f, err := garc.Parse(data)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		c, _ := hdr.ParseContainer(f)
		sei, _ := hdr.ParseSEI(f)
		return validate.Build(f, c, sei, "deadbeef")
	}
	t.Fatalf("scenario %s missing", name)
	return nil
}

func TestValidReportClean(t *testing.T) {
	rep := buildReport(t, "hdr-valid.garc")
	if rep.Summary.VideoTrackCount != 3 {
		t.Errorf("track count=%d", rep.Summary.VideoTrackCount)
	}
	if rep.Summary.ConflictCount != 0 || rep.Summary.ErrorCount != 0 || rep.Summary.MissingCount != 0 {
		t.Errorf("valid file not clean: %+v", rep.Summary)
	}
	tr := rep.Tracks[0]
	if !tr.Mastering.Present || tr.Mastering.Source != hdr.SourceContainer || tr.Mastering.Status != validate.StatusOK {
		t.Errorf("track1 mastering wrong: %+v", tr.Mastering)
	}
	if tr.MaxCLL.Nits != 1000 {
		t.Errorf("MaxCLL=%d", tr.MaxCLL.Nits)
	}
}

func TestMixedConflictAndMissing(t *testing.T) {
	rep := buildReport(t, "hdr-mixed.garc")
	if rep.Summary.ConflictCount != 1 {
		t.Errorf("conflict=%d want 1", rep.Summary.ConflictCount)
	}
	if rep.Summary.MissingCount != 1 {
		t.Errorf("missing=%d want 1", rep.Summary.MissingCount)
	}
	if rep.Fatal {
		t.Error("mixed file must not be fatal")
	}
	// 轨2 MaxFALL 冲突，双值都保留。
	var tr2 *validate.TrackReport
	for i := range rep.Tracks {
		if rep.Tracks[i].TrackID == 2 {
			tr2 = &rep.Tracks[i]
		}
	}
	if tr2 == nil || tr2.MaxFALL.Status != validate.StatusConflict {
		t.Fatalf("track2 MaxFALL not conflict: %+v", tr2)
	}
	if tr2.MaxFALL.ContainerValue == nil || *tr2.MaxFALL.ContainerValue != 400 ||
		len(tr2.MaxFALL.SEIValues) == 0 || tr2.MaxFALL.SEIValues[0].Nits != 250 {
		t.Errorf("conflict must keep both values: %+v", tr2.MaxFALL)
	}
	// 冲突不允许被默认值覆盖：有效值仍是容器的 400。
	if tr2.MaxFALL.Nits != 400 {
		t.Errorf("effective MaxFALL=%d, container value must win", tr2.MaxFALL.Nits)
	}
}

func TestInheritedStatus(t *testing.T) {
	rep := buildReport(t, "hdr-inherited.garc")
	tr := rep.Tracks[0]
	if tr.Mastering.Status != validate.StatusInherited || tr.Mastering.Source != hdr.SourceSEI {
		t.Errorf("mastering not inherited: %+v", tr.Mastering)
	}
	if tr.MaxCLL.Status != validate.StatusInherited || tr.MaxCLL.Source != hdr.SourceSEI {
		t.Errorf("MaxCLL not inherited: %+v", tr.MaxCLL)
	}
}

func TestMissingFieldsScenario(t *testing.T) {
	rep := buildReport(t, "hdr-missing-fields.garc")
	tr := rep.Tracks[0]
	if tr.MaxCLL.Status != validate.StatusMissing || tr.MaxCLL.Present {
		t.Errorf("absent CLL must be missing/present=false: %+v", tr.MaxCLL)
	}
	if !containsCode(tr.Issues, validate.CodeFieldMissing) {
		t.Errorf("want FIELD_MISSING: %+v", tr.Issues)
	}
}

func TestIssueSortingStable(t *testing.T) {
	rep := buildReport(t, "hdr-mixed.garc")
	for _, tr := range rep.Tracks {
		for i := 1; i < len(tr.Issues); i++ {
			a, b := tr.Issues[i-1], tr.Issues[i]
			if a.Offset > b.Offset || (a.Offset == b.Offset && a.Code > b.Code) {
				t.Fatalf("issues not sorted: %s before %s", a.Code, b.Code)
			}
		}
	}
	if !strings.HasPrefix(rep.Summary.InputSHA256, "deadbeef") {
		t.Error("sha256 not threaded into summary")
	}
}

func containsCode(list []hdr.Issue, code string) bool {
	for _, is := range list {
		if is.Code == code {
			return true
		}
	}
	return false
}
