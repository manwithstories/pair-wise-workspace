package mapping

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"mediamerge/internal/demux"
	"mediamerge/internal/fixture"
	"mediamerge/internal/model"
)

func open(t *testing.T) *demux.Reader {
	t.Helper()
	path := filepath.Join(t.TempDir(), "in.mp4")
	if err := os.WriteFile(path, fixture.DefaultPlan(3, 8).Build(), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := demux.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

func TestDefaultRulesPreserveOrder(t *testing.T) {
	r := open(t)
	plan, err := Apply(r.Tracks, DefaultRules(r.Tracks))
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Tracks) != 3 {
		t.Fatalf("tracks=%d", len(plan.Tracks))
	}
	for i, tr := range plan.Tracks {
		if tr.TargetID != uint32(i+1) {
			t.Fatalf("track %d target=%d", i, tr.TargetID)
		}
	}
	if plan.TotalSamples() != 24 {
		t.Fatalf("samples=%d", plan.TotalSamples())
	}
}

func TestParseRulesAndApply(t *testing.T) {
	r := open(t)
	rules, err := ParseRules("1=10:video,2=11:audio,3=12:video")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := Apply(r.Tracks, rules)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Map[10].MediaType != model.MediaVideo || plan.Map[11].MediaType != model.MediaAudio {
		t.Fatalf("unexpected media mapping")
	}
	if plan.OutputTimescale() < 30000 {
		t.Fatalf("timescale=%d", plan.OutputTimescale())
	}
}

func TestDuplicateTargetFails(t *testing.T) {
	r := open(t)
	rules, err := ParseRules("1=5:video,2=5:audio")
	if err != nil {
		t.Fatal(err)
	}
	_, err = Apply(r.Tracks, rules)
	if err == nil {
		t.Fatal("duplicate target accepted")
	}
	pe := err.(*model.Error)
	if pe.Stage != model.StageMapping || pe.Code != model.CodeMapping {
		t.Fatalf("err=%v", pe)
	}
}

func TestMissingRequiredTrackFails(t *testing.T) {
	r := open(t)
	rules := []Rule{{Input: "99", Target: 1, Required: true}}
	if _, err := Apply(r.Tracks, rules); err == nil {
		t.Fatal("missing required track accepted")
	}
	// A non-required missing track is skipped.
	rules = []Rule{{Input: "99", Target: 1}, {Input: "1", Target: 2, Required: true}}
	plan, err := Apply(r.Tracks, rules)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Tracks) != 1 || plan.Tracks[0].TargetID != 2 {
		t.Fatalf("plan=%+v", plan.Tracks)
	}
}

func TestMediaMismatchFails(t *testing.T) {
	r := open(t)
	rules := []Rule{{Input: "1", Target: 1, Media: model.MediaAudio, Required: true}}
	if _, err := Apply(r.Tracks, rules); err == nil {
		t.Fatal("media mismatch accepted")
	}
}

func TestParseRulesRejectsGarbage(t *testing.T) {
	for _, spec := range []string{"1", "1=x:video", "1=0:video", "1=5:music"} {
		if _, err := ParseRules(spec); err == nil {
			t.Fatalf("spec %q accepted", spec)
		}
	}
}
