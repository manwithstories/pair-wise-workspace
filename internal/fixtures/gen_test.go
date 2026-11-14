package fixtures

import (
	"os"
	"path/filepath"
	"testing"

	"subtool/internal/encsniff"
	"subtool/internal/subtitle"
)

func TestGenerateCreatesParseableDeterministicFixtures(t *testing.T) {
	dir := t.TempDir()
	summary, err := Generate(dir, 256, 7)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Files != 14 || summary.OverlapGroups != 7 {
		t.Fatalf("unexpected summary: %#v", summary)
	}
	stress, err := os.ReadFile(filepath.Join(dir, "stress_50k.srt"))
	if err != nil {
		t.Fatal(err)
	}
	track, err := subtitle.ParseSRT(stress)
	if err != nil {
		t.Fatal(err)
	}
	if len(track.Cues) != 256 || subtitle.OverlapGroupCount(track.Cues) != 7 {
		t.Fatalf("stress cues=%d groups=%d", len(track.Cues), subtitle.OverlapGroupCount(track.Cues))
	}
	gbData, err := os.ReadFile(filepath.Join(dir, "mixed_gb18030.srt"))
	if err != nil {
		t.Fatal(err)
	}
	enc, err := encsniff.Detect(gbData)
	if err != nil {
		t.Fatal(err)
	}
	if enc.Name != "GB18030" || enc.Confidence < encsniff.LowConfidence {
		t.Fatalf("mixed encoding: %#v", enc)
	}
	gbTrack, err := subtitle.ParseSRT(enc.Text)
	if err != nil || len(gbTrack.Cues) != 256 {
		t.Fatalf("mixed parse: cues=%d err=%v", len(gbTrack.Cues), err)
	}
	ep01, err := os.ReadFile(filepath.Join(dir, "ep01.ass"))
	if err != nil {
		t.Fatal(err)
	}
	assTrack, err := subtitle.ParseASS(ep01)
	if err != nil {
		t.Fatal(err)
	}
	if assTrack.Cues[0].StartMS != 1250 || assTrack.Cues[0].EndMS != 3750 {
		t.Fatalf("ep01 first cue: %#v", assTrack.Cues[0])
	}
	for _, name := range []string{
		"invalid.srt", "low_confidence.srt", "truncated.srt", "missing_events.ass",
		"short_dialogue.ass", "timeline_overflow.vtt", "ass_drawing.ass",
		"mixed_utf8_bom.srt", "mixed_big5.vtt", "legacy_shiftjis.ass",
	} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	second := t.TempDir()
	if _, err := Generate(second, 256, 7); err != nil {
		t.Fatal(err)
	}
	again, err := os.ReadFile(filepath.Join(second, "stress_50k.srt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(again) != string(stress) {
		t.Fatal("same seed produced different bytes")
	}
}
