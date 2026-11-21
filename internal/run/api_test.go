package run

import (
	"context"
	"testing"

	"github.com/mediamend/mediamend/internal/pipeline"
	"github.com/mediamend/mediamend/internal/segment"
)

// TestLibraryAPIUsableWithoutCLI exercises the documented library entry
// points the way an embedding application would: progress events, a custom
// ladder and access to the structured report.
func TestLibraryAPIUsableWithoutCLI(t *testing.T) {
	in, _ := fixture(t, 150, 0)
	out := t.TempDir()
	ladder, err := segment.ParseLadder("360p,720p")
	if err != nil {
		t.Fatal(err)
	}
	if got := segment.FormatLadderNames(ladder); got != "360p,720p" {
		t.Fatalf("ladder names=%q", got)
	}
	var events []pipeline.Event
	rep, err := Run(context.Background(), Config{
		InputDir: in, OutDir: out, Ladder: ladder, RandomSeeks: 25,
		Progress: func(e pipeline.Event) { events = append(events, e) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 {
		t.Fatal("library API delivered no progress events")
	}
	seenStages := map[string]bool{}
	for _, e := range events {
		seenStages[e.Stage] = true
	}
	for _, want := range []string{pipeline.StageParse, pipeline.StageReorder,
		pipeline.StageTransform, pipeline.StageVerify} {
		if !seenStages[want] {
			t.Fatalf("no progress event for stage %s (saw %v)", want, seenStages)
		}
	}
	if rep.FramesCommitted == 0 || rep.Segments == 0 {
		t.Fatalf("report=%+v", rep)
	}
	// Six cameras, two renditions plus one combined manifest each.
	if len(rep.Manifests) != 6*3 {
		t.Fatalf("manifests=%d want 18", len(rep.Manifests))
	}
}
