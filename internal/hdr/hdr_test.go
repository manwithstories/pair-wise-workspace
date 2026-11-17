package hdr_test

import (
	"testing"

	"hdrcheck/internal/fixture"
	"hdrcheck/internal/garc"
	"hdrcheck/internal/hdr"
)

func parseScenario(t *testing.T, name string) *garc.File {
	t.Helper()
	for _, sc := range fixture.Scenarios() {
		if sc.Name != name {
			continue
		}
		b, err := sc.Build()
		if err != nil {
			t.Fatalf("build %s: %v", name, err)
		}
		data, err := b.Build()
		if err != nil {
			t.Fatalf("assemble %s: %v", name, err)
		}
		f, err := garc.Parse(data)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		return f
	}
	t.Fatalf("scenario %s not found", name)
	return nil
}

func hasCode(issues []hdr.Issue, code string) bool {
	for _, is := range issues {
		if is.Code == code {
			return true
		}
	}
	return false
}

func TestParseContainerValid(t *testing.T) {
	f := parseScenario(t, "hdr-valid.garc")
	c, err := hdr.ParseContainer(f)
	if err != nil {
		t.Fatalf("ParseContainer: %v", err)
	}
	if c.Mastering == nil || !c.Mastering.Present || c.Mastering.Source != hdr.SourceContainer {
		t.Fatal("mastering display not parsed from container")
	}
	if c.CLL == nil || c.CLL.MaxContentLightLevel.Nits != 1000 ||
		c.CLL.MaxFrameAverageLightLevel.Nits != 400 {
		t.Fatalf("CLL parsed wrong: %+v", c.CLL)
	}
	if len(c.Issues) != 0 {
		t.Errorf("valid container produced issues: %+v", c.Issues)
	}
}

func TestParseContainerDuplicate(t *testing.T) {
	f := parseScenario(t, "hdr-duplicate.garc")
	c, _ := hdr.ParseContainer(f)
	if !hasCode(c.Issues, "DUPLICATE_FIELD") {
		t.Errorf("want DUPLICATE_FIELD, got %+v", c.Issues)
	}
	// 首次值仍保留。
	if c.Mastering == nil || !c.Mastering.Present {
		t.Error("first mastering value should survive duplicate")
	}
}

func TestMissingFieldNeverZero(t *testing.T) {
	// missing 场景无 CLL TLV。
	f := parseScenario(t, "hdr-missing-fields.garc")
	c, _ := hdr.ParseContainer(f)
	if c.CLL != nil {
		t.Error("absent CLL must stay nil, not a zero-value struct")
	}
	ml := hdr.MissingLightLevel(0)
	if ml.Present || ml.Source != hdr.SourceMissing || ml.Nits != 0 {
		t.Error("MissingLightLevel must be present=false/source=missing")
	}
}

func TestSEIChecksumErrorDropsRecord(t *testing.T) {
	f := parseScenario(t, "hdr-badchecksum.garc")
	sei, err := hdr.ParseSEI(f)
	if err != nil {
		t.Fatalf("ParseSEI: %v", err)
	}
	if !hasCode(sei.Issues, "CHECKSUM_ERROR") {
		t.Errorf("want CHECKSUM_ERROR, got %+v", sei.Issues)
	}
	// 损坏的是 track 1 的第一条 SEI（mastering），该记录必须被丢弃。
	for _, rec := range sei.Records {
		if rec.TrackID == 1 && rec.Mastering != nil {
			t.Error("checksum-failed mastering record must not survive")
		}
	}
}

func TestSEIOverlap(t *testing.T) {
	f := parseScenario(t, "hdr-overlap.garc")
	sei, _ := hdr.ParseSEI(f)
	if !hasCode(sei.Issues, "RECORDS_OVERLAP") {
		t.Errorf("want RECORDS_OVERLAP, got %+v", sei.Issues)
	}
}

func TestSEIInheritedFromBigEndian(t *testing.T) {
	f := parseScenario(t, "hdr-inherited.garc")
	sei, err := hdr.ParseSEI(f)
	if err != nil {
		t.Fatalf("ParseSEI: %v", err)
	}
	if len(sei.RecordsOf(1)) != 2 {
		t.Fatalf("track 1 SEI records=%d, want 2", len(sei.RecordsOf(1)))
	}
	var sawCLL1000 bool
	for _, rec := range sei.RecordsOf(1) {
		if rec.CLL != nil && rec.CLL.MaxContentLightLevel.Nits == 1000 {
			sawCLL1000 = true
		}
	}
	if !sawCLL1000 {
		t.Error("big-endian MaxCLL=1000 not decoded correctly")
	}
}

func TestRangeChecks(t *testing.T) {
	if hdr.ValidateCLL(0) || hdr.ValidateCLL(10001) {
		t.Error("CLL boundary check wrong")
	}
	if !hdr.ValidateCLL(1) || !hdr.ValidateCLL(10000) {
		t.Error("CLL inclusive boundaries wrong")
	}
	if hdr.ValidateChromaticity(50001) || !hdr.ValidateChromaticity(50000) {
		t.Error("chromaticity boundary wrong")
	}
}
