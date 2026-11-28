package subtitle

import (
	"strings"
	"testing"

	"presspack/internal/core"
)

func TestRenderVTTRoundTrip(t *testing.T) {
	cues := []Cue{
		{Start: core.FromMillis(0), End: core.FromMillis(1500), Text: "hello"},
		{Start: core.FromMillis(1500), End: core.FromMillis(2000), Text: "world"},
	}
	doc := RenderVTT(cues)
	if !strings.HasPrefix(string(doc), "WEBVTT\n") {
		t.Fatal("missing VTT header")
	}
	back, faults := Parse(doc)
	if len(faults) != 0 || len(back) != 2 {
		t.Fatalf("round trip: cues=%v faults=%v", back, faults)
	}
	if back[0].End.Millis() != 1500 || back[1].Text != "world" {
		t.Errorf("round trip cues = %+v", back)
	}
}

func TestRenderVTTEmpty(t *testing.T) {
	doc := RenderVTT(nil)
	if strings.TrimSpace(string(doc)) != "WEBVTT" {
		t.Errorf("empty render = %q", doc)
	}
}
