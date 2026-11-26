package transform

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"bq/internal/clock"
	"bq/internal/ffmpeg"
	"bq/internal/fixture"
	"bq/internal/manifest"
	"bq/internal/model"
	"bq/internal/timeline"
)

func requireFFmpeg(t *testing.T) context.Context {
	t.Helper()
	if _, _, err := ffmpeg.Binaries(); err != nil {
		t.Skipf("ffmpeg unavailable: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// shortFixture builds a small generated run on disk and returns its parsed
// manifest plus a composed timeline.
func shortFixture(t *testing.T, dir string) (*manifest.Manifest, *timeline.Timeline) {
	t.Helper()
	opts := fixture.Options{
		Seed: 5, DurationS: 12, LossRate: 0, InjectWrap: false,
		PacketCadenceS: 1, GroupSize: 5,
	}
	m := fixture.Build(opts)
	ctx := requireFFmpeg(t)
	if err := fixture.Generate(ctx, m, dir); err != nil {
		t.Fatal(err)
	}
	perStream := map[int][]manifest.Packet{}
	for _, s := range m.Streams {
		perStream[s.ID] = m.PacketsForStream(s.ID)
	}
	tl, errs := timeline.Compose(m, perStream,
		map[int]*clock.WrapExpander{}, map[int][]clock.WrapEvent{})
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	return m, tl
}

func TestRemuxHealthyTracks(t *testing.T) {
	ctx := requireFFmpeg(t)
	dir := t.TempDir()
	m, tl := shortFixture(t, dir)
	r := New(m, tl, dir)
	outcomes, errs := r.Run(ctx)
	if len(errs) != 0 {
		t.Fatalf("healthy remux must not error: %v", errs)
	}
	if len(outcomes) != len(m.Streams) {
		t.Fatalf("want %d outcomes, got %d", len(m.Streams), len(outcomes))
	}
	for _, oc := range outcomes {
		if oc.Skipped || oc.Segments == 0 {
			t.Fatalf("stream %d expected segments, got %+v", oc.StreamID, oc)
		}
		if info, err := os.Stat(oc.OutputPath); err != nil || info.Size() == 0 {
			t.Fatalf("remux output missing for stream %d", oc.StreamID)
		}
	}
}

func TestRemuxMissingTrackContinuesOthers(t *testing.T) {
	ctx := requireFFmpeg(t)
	dir := t.TempDir()
	m, tl := shortFixture(t, dir)
	// Delete one video source; the other three tracks must still succeed.
	victim := m.Streams[1]
	victimPath := filepath.Join(dir, "media", filepath.Base(victim.File))
	if err := os.Remove(victimPath); err != nil {
		t.Fatal(err)
	}
	r := New(m, tl, dir)
	outcomes, errs := r.Run(ctx)
	var missingRec *model.Error
	success := 0
	for _, e := range errs {
		if e.Code == model.ErrMissingTrack && e.StreamID == victim.ID {
			missingRec = &e
		}
	}
	if missingRec == nil {
		t.Fatalf("missing track not reported: %v", errs)
	}
	for _, oc := range outcomes {
		if !oc.Skipped && oc.Segments > 0 {
			success++
		}
	}
	if success != len(m.Streams)-1 {
		t.Fatalf("want %d surviving tracks, got %d", len(m.Streams)-1, success)
	}
}

func TestRemuxOutOfBoundsEditRecorded(t *testing.T) {
	ctx := requireFFmpeg(t)
	dir := t.TempDir()
	m, tl := shortFixture(t, dir)
	// Push an edit beyond the recovered tail of stream 0.
	items := tl.Streams[0]
	last := items[len(items)-1].MasterPTS
	tl.Edits[0] = append(tl.Edits[0], timeline.EditSegment{
		StartMaster: last + 100*clock.MasterHz, EndMaster: last + 200*clock.MasterHz,
		StartSource: 999_000, EndSource: 999_999, InBounds: false,
	})
	r := New(m, tl, dir)
	_, errs := r.Run(ctx)
	found := false
	for _, e := range errs {
		if e.Code == model.ErrEditOutOfBounds && e.StreamID == 0 {
			found = true
			if e.SourceOffset != 999_000 || !e.HasPTS {
				t.Fatalf("out-of-bounds record lost provenance: %+v", e)
			}
		}
	}
	if !found {
		t.Fatalf("out-of-bounds edit not reported: %v", errs)
	}
	// The valid segments of the same stream must not raise other errors.
	for _, e := range errs {
		if e.Stage == model.StageTransform && e.StreamID == 0 && e.Code != model.ErrEditOutOfBounds {
			t.Fatalf("unexpected transform error on stream 0: %v", e)
		}
	}
}

func TestRemuxCreatesOutputInsideBaseDir(t *testing.T) {
	ctx := requireFFmpeg(t)
	dir := t.TempDir()
	m, tl := shortFixture(t, dir)
	r := New(m, tl, dir)
	outcomes, _ := r.Run(ctx)
	for _, oc := range outcomes {
		if oc.OutputPath == "" {
			continue
		}
		rel, err := filepath.Rel(dir, oc.OutputPath)
		if err != nil || len(rel) >= 2 && rel[:2] == ".." {
			t.Fatalf("output escaped base dir: %s", oc.OutputPath)
		}
	}
}
