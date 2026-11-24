package manifest

import (
	"strings"
	"testing"

	"mediadeliver/internal/model"
)

const goodManifest = `
# concert night one
MATERIAL cam1 fixtures/cam1.mp4 mp4
TRACK cam1 1 video avc1
TRACK cam1 2 audio mp4a
MATERIAL rec1 fixtures/rec1.rtp rtp
TRACK rec1 11 audio mp4a
EDIT segA cam1 1 1000 500 90000
EDIT segA cam1 2 1000 500 48000
MAP segA video cam1 1
MAP segA audio cam1 2
EDIT segB rec1 11 0 480 48000
MAP segB audio rec1 11
`

func TestParseGoodManifest(t *testing.T) {
	m, err := Parse(strings.NewReader(goodManifest))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(m.Materials) != 2 || len(m.Edits) != 3 || len(m.Mappings) != 3 {
		t.Fatalf("counts wrong: %+v", m)
	}
	if len(m.SegmentOrder) != 2 || m.SegmentOrder[0] != "segA" || m.SegmentOrder[1] != "segB" {
		t.Fatalf("segment order: %v", m.SegmentOrder)
	}
	e := m.Edits[0]
	start, err := e.StartTicks()
	if err != nil || start != 1000 {
		t.Fatalf("start ticks: %d %v", start, err)
	}
	end, err := e.EndTicks()
	if err != nil || end != 1500 {
		t.Fatalf("end ticks: %d %v", end, err)
	}
	if spec, ok := m.FindTrack("cam1", 1); !ok || spec.Codec != "avc1" {
		t.Fatalf("find track: %+v %v", spec, ok)
	}
}

func TestParseReportsLineAndKind(t *testing.T) {
	cases := map[string]string{
		"missing track": `
MATERIAL cam1 fixtures/cam1.mp4 mp4
EDIT segA cam1 9 0 10 90000
`,
		"illegal time base": `
MATERIAL cam1 fixtures/cam1.mp4 mp4
TRACK cam1 1 video avc1
EDIT segA cam1 1 0 10 0
`,
		"negative duration": `
MATERIAL cam1 fixtures/cam1.mp4 mp4
TRACK cam1 1 video avc1
EDIT segA cam1 1 0 -10 90000
MAP segA video cam1 1
`,
		"duplicate edit interval": `
MATERIAL cam1 fixtures/cam1.mp4 mp4
TRACK cam1 1 video avc1
EDIT segA cam1 1 0 100 90000
EDIT segA cam1 1 50 100 90000
MAP segA video cam1 1
`,
		"duplicate mapping": `
MATERIAL cam1 fixtures/cam1.mp4 mp4
TRACK cam1 1 video avc1
EDIT segA cam1 1 0 100 90000
MAP segA video cam1 1
MAP segA video cam1 1
`,
		"missing mapping": `
MATERIAL cam1 fixtures/cam1.mp4 mp4
TRACK cam1 1 video avc1
EDIT segA cam1 1 0 100 90000
`,
	}
	wantKind := map[string]model.Kind{
		"missing track":           model.KindInvalidInput,
		"illegal time base":       model.KindInvalidInput,
		"negative duration":       model.KindInvalidInput,
		"duplicate edit interval": model.KindConflict,
		"duplicate mapping":       model.KindConflict,
		"missing mapping":         model.KindInvalidInput,
	}
	for name, src := range cases {
		_, err := Parse(strings.NewReader(src))
		if err == nil {
			t.Fatalf("%s: expected error", name)
		}
		if model.KindOf(err) != wantKind[name] {
			t.Fatalf("%s: kind=%v want %v (%v)", name, model.KindOf(err), wantKind[name], err)
		}
		if model.PositionOf(err) <= 0 {
			t.Fatalf("%s: error missing line number: %v", name, err)
		}
	}
}

func TestParseRejectsUnknownFormatAndDirective(t *testing.T) {
	if _, err := Parse(strings.NewReader("MATERIAL x f/x mov\n")); model.KindOf(err) != model.KindInvalidInput {
		t.Fatalf("format: %v", err)
	}
	if _, err := Parse(strings.NewReader("WAT x\n")); model.KindOf(err) != model.KindInvalidInput {
		t.Fatalf("directive: %v", err)
	}
	if _, err := Parse(strings.NewReader("# only comment\n")); model.KindOf(err) != model.KindInvalidInput {
		t.Fatalf("empty: %v", err)
	}
}

func TestTouchingRangesAllowed(t *testing.T) {
	src := `
MATERIAL cam1 fixtures/cam1.mp4 mp4
TRACK cam1 1 video avc1
EDIT segA cam1 1 0 100 90000
EDIT segA cam1 1 100 100 90000
MAP segA video cam1 1
`
	if _, err := Parse(strings.NewReader(src)); err != nil {
		t.Fatalf("touching ranges should be allowed: %v", err)
	}
}
