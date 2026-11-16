// End-to-end golden tests: fixture generation → parse/index → timeline →
// seek → hash → export, plus fault-injection paths. (The task brief
// calls this testdata/golden_test.go; it lives at the repo root because
// the go tool skips directories named testdata when matching ./....)
package mp4loc

import (
	"crypto/sha256"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"mp4loc/internal/box"
	"mp4loc/internal/export"
	"mp4loc/internal/fixture"
	"mp4loc/internal/hash"
	"mp4loc/internal/seek"
)

func genFixture(t *testing.T, dir string, p fixture.Params) string {
	t.Helper()
	if p.Out == "" {
		p.Out = filepath.Join(dir, "sample.mp4")
	}
	if p.VideoFrames == 0 {
		p.VideoFrames = 240
	}
	if p.AudioFrames == 0 {
		p.AudioFrames = 480
	}
	if err := fixture.Generate(p); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	return p.Out
}

func analyze(t *testing.T, path string) *export.Analysis {
	t.Helper()
	a, err := export.Analyze(path)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	t.Cleanup(a.Close)
	return a
}

func TestGoldenInspectFields(t *testing.T) {
	dir := t.TempDir()
	path := genFixture(t, dir, fixture.Params{EditList: true, TimestampWrap: true, Seed: 42})
	a := analyze(t, path)

	r := a.Report
	if r.BoxCount < 20 || r.BoxCount > 80 {
		t.Fatalf("BOX_COUNT %d outside [20,80]", r.BoxCount)
	}
	if r.TrackCount != 2 {
		t.Fatalf("TRACK_COUNT %d, want 2", r.TrackCount)
	}
	if r.VideoTrackID != 1 {
		t.Fatalf("VIDEO_TRACK_ID %d, want 1", r.VideoTrackID)
	}
	v := r.Tracks[0]
	if v.SampleCount != 240 {
		t.Fatalf("SAMPLE_COUNT %d, want 240", v.SampleCount)
	}
	if v.KeyframeCount < 20 {
		t.Fatalf("KEYFRAME_COUNT %d, want >= 20", v.KeyframeCount)
	}
	if v.EditSegmentCount != 2 {
		t.Fatalf("EDIT_SEGMENT_COUNT %d, want 2", v.EditSegmentCount)
	}
	if v.WrapEvents < 1 {
		t.Fatalf("WRAP_EVENTS %d, want >= 1", v.WrapEvents)
	}
	// Text and JSON views come from the same analysis.
	text := r.Text()
	for _, key := range []string{"BOX_COUNT", "TRACK_COUNT", "VIDEO_TRACK_ID", "TIMESCALE",
		"SAMPLE_COUNT", "KEYFRAME_COUNT", "EDIT_SEGMENT_COUNT", "WRAP_EVENTS"} {
		if !strings.Contains(text, key+":") {
			t.Fatalf("text report missing %s", key)
		}
	}
	if _, err := r.JSON(); err != nil {
		t.Fatalf("JSON: %v", err)
	}
}

func TestGoldenDeterministic(t *testing.T) {
	dir := t.TempDir()
	p1 := genFixture(t, dir, fixture.Params{Out: filepath.Join(dir, "a.mp4"), EditList: true, TimestampWrap: true, Seed: 42})
	p2 := genFixture(t, dir, fixture.Params{Out: filepath.Join(dir, "b.mp4"), EditList: true, TimestampWrap: true, Seed: 42})
	h := func(p string) [32]byte {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		return sha256.Sum256(b)
	}
	if h(p1) != h(p2) {
		t.Fatal("same seed produced different files")
	}
}

func TestGoldenSeekAndHash(t *testing.T) {
	dir := t.TempDir()
	path := genFixture(t, dir, fixture.Params{EditList: true, TimestampWrap: true, Seed: 42})
	a := analyze(t, path)
	tr := a.Movie.TrackByID(1)
	tl := a.Timelines[1]

	landing, err := seek.Locate(tl, 12.345, 2, 3)
	if err != nil {
		t.Fatalf("Locate: %v", err)
	}
	if !tr.Table.Samples[landing.Sample].Keyframe {
		t.Fatalf("landing sample %d is not a keyframe", landing.Sample)
	}
	if len(landing.Window) != 6 {
		t.Fatalf("window %d, want 6", len(landing.Window))
	}
	recs, err := hash.Samples(a.File, a.FileSize, tr, tl, landing.Window, true, 4,
		func(s *box.SampleRecord) string { return seek.FrameType(s, true, true) })
	if err != nil {
		t.Fatalf("Samples: %v", err)
	}
	hexRe := regexp.MustCompile(`^[0-9a-f]{64}$`)
	for _, r := range recs {
		if !hexRe.MatchString(r.SHA256) {
			t.Fatalf("sample %d bad hash %q", r.Index, r.SHA256)
		}
	}
	// Landing keyframe must be the nearest keyframe at/below the target:
	// the next keyframe's PTS must exceed the mapped target.
	keys := tr.Table.Keyframes
	for i, k := range keys {
		if k == landing.Sample && i+1 < len(keys) {
			next := tr.Table.Samples[keys[i+1]]
			if next.PTS <= landing.PTS {
				t.Fatal("keyframe PTS not increasing")
			}
		}
	}
}

func TestGoldenSeekBoundaries(t *testing.T) {
	dir := t.TempDir()
	path := genFixture(t, dir, fixture.Params{EditList: true, Seed: 7})
	a := analyze(t, path)
	tl := a.Timelines[1]

	cases := []struct {
		sec    float64
		reason string
	}{
		{0.2, "empty_edit"},
		{99.0, "beyond_duration"},
		{-1.0, "before_first_segment"},
	}
	for _, c := range cases {
		_, err := seek.Locate(tl, c.sec, 0, 0)
		le, ok := err.(*seek.LocateError)
		if !ok || le.Reason != c.reason {
			t.Fatalf("seek %.2f: got %v, want reason %s", c.sec, err, c.reason)
		}
	}
}

func TestGoldenExportJSONL(t *testing.T) {
	dir := t.TempDir()
	path := genFixture(t, dir, fixture.Params{EditList: true, TimestampWrap: true, Seed: 42})
	a := analyze(t, path)
	out := filepath.Join(dir, "report.jsonl")
	f, err := os.Create(out)
	if err != nil {
		t.Fatal(err)
	}
	if err := export.WriteJSONL(f, a); err != nil {
		t.Fatalf("WriteJSONL: %v", err)
	}
	f.Close()

	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	// 1 summary + 2 tracks + 24 keyframes + 1 wrap event.
	if len(lines) != 28 {
		t.Fatalf("jsonl lines %d, want 28", len(lines))
	}
	var first map[string]interface{}
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("line 0 not JSON: %v", err)
	}
	if first["type"] != "summary" {
		t.Fatalf("line 0 type %v", first["type"])
	}
	for i, ln := range lines {
		var m map[string]interface{}
		if err := json.Unmarshal([]byte(ln), &m); err != nil {
			t.Fatalf("line %d invalid JSON: %v", i, err)
		}
	}
}

func TestGoldenFaultInjection(t *testing.T) {
	dir := t.TempDir()

	// no-stss: video seek must refuse rather than assume decodability.
	p := genFixture(t, dir, fixture.Params{Out: filepath.Join(dir, "nostss.mp4"), Inject: "no-stss", Seed: 42})
	a := analyze(t, p)
	if a.Movie.TrackByID(1).Table.HasSTSS {
		t.Fatal("no-stss fixture still has stss")
	}
	if _, err := seek.Locate(a.Timelines[1], 5.0, 0, 0); err == nil {
		t.Fatal("seek without stss should fail")
	} else if le, ok := err.(*seek.LocateError); !ok || le.Reason != "no_keyframe_table" {
		t.Fatalf("reason = %v", err)
	}
	a.Close()

	// Structural injections: Analyze must reject with a StructureError.
	for _, inject := range []string{"truncate", "bad-offset", "bad-elst"} {
		p := genFixture(t, dir, fixture.Params{
			Out: filepath.Join(dir, inject+".mp4"), Inject: inject, EditList: true, Seed: 42})
		if _, err := export.Analyze(p); err == nil {
			t.Fatalf("inject %s: expected error", inject)
		} else if !box.IsStructure(err) {
			t.Fatalf("inject %s: want StructureError, got %v", inject, err)
		}
	}
}
