package manifest

import (
	"strings"
	"testing"

	"github.com/pairwise/mediad/internal/media"
)

func dashFixture() []byte {
	return []byte(`<?xml version="1.0" encoding="UTF-8"?>
<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="static" mediaPresentationDuration="PT24S" minBufferTime="PT2S">
  <Period id="0" duration="PT24S">
    <AdaptationSet contentType="video">
      <SegmentTemplate timescale="90000" initialization="init-$RepresentationID$.mp4" media="seg-$RepresentationID$-$Number%03d$.m4s" startNumber="1" duration="180000"/>
      <Representation id="high" bandwidth="5000000" width="1920" height="1080" codecs="avc1.640028" mimeType="video/mp4"/>
      <Representation id="low" bandwidth="800000" width="640" height="360" codecs="avc1.42c01e" mimeType="video/mp4"/>
      <Representation id="mid" bandwidth="2200000" width="1280" height="720" codecs="avc1.4d401f" mimeType="video/mp4"/>
    </AdaptationSet>
  </Period>
</MPD>`)
}

func TestDASHRoundTripSortsThreeTiers(t *testing.T) {
	canonical, original, reparsed, mismatch, err := RoundTripDASH(dashFixture())
	if err != nil {
		t.Fatal(err)
	}
	if mismatch != nil {
		t.Fatalf("unexpected mismatch: %s", mismatch)
	}
	bws := []int{
		reparsed.Period.AdaptationSet.Representations[0].Bandwidth,
		reparsed.Period.AdaptationSet.Representations[1].Bandwidth,
		reparsed.Period.AdaptationSet.Representations[2].Bandwidth,
	}
	if bws[0] != 800000 || bws[1] != 2200000 || bws[2] != 5000000 {
		t.Fatalf("bandwidth order=%v", bws)
	}
	if CompareDASH(original, reparsed) != nil && mismatch == nil {
		t.Fatal("normalized comparison should be stable")
	}
	if !strings.Contains(string(canonical), `bandwidth="800000"`) {
		t.Fatal("canonical DASH missing low tier")
	}
}

func TestDASHRejectsMissingBandwidthAndReportsField(t *testing.T) {
	bad := []byte(strings.Replace(string(dashFixture()), `bandwidth="2200000"`, ``, 1))
	_, err := ParseDASH(bad)
	me, ok := media.AsError(err)
	if !ok || me.Code != media.CodeMissingField {
		t.Fatalf("expected structured missing field, got %v", err)
	}
	if !strings.Contains(me.Field, "bandwidth") {
		t.Fatalf("field=%q", me.Field)
	}
}

func TestDASHFirstMismatchLocator(t *testing.T) {
	a, err := ParseDASH(dashFixture())
	if err != nil {
		t.Fatal(err)
	}
	b, err := ParseDASH(dashFixture())
	if err != nil {
		t.Fatal(err)
	}
	b.Period.AdaptationSet.Representations[1].Codecs = "avc1.damaged"
	mm := CompareDASH(a, b)
	if mm == nil || mm.Field != "Representation[1].codecs" {
		t.Fatalf("mismatch=%+v", mm)
	}
}

func TestDASHRejectsTruncatedXML(t *testing.T) {
	truncated := dashFixture()[:len(dashFixture())/2]
	if _, err := ParseDASH(truncated); err == nil {
		t.Fatal("truncated XML must be rejected")
	} else if media.ExitCode(err) != 2 {
		t.Fatalf("exit=%d", media.ExitCode(err))
	}
}
