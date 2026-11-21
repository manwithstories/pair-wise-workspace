package summarize

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mediamerge/internal/demux"
	"mediamerge/internal/fixture"
	"mediamerge/internal/mapping"
	"mediamerge/internal/txn"
)

func build(t *testing.T) (dir string, in *demux.Reader, out *demux.Reader) {
	t.Helper()
	dir = t.TempDir()
	path := filepath.Join(dir, "input.mp4")
	if err := os.WriteFile(path, fixture.DefaultPlan(3, 12).Build(), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := demux.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	plan, err := mapping.Apply(r.Tracks, mapping.DefaultRules(r.Tracks))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := txn.Run(context.Background(), dir, r, plan, txn.Options{Queue: 2, InterruptAfter: -1}); err != nil {
		t.Fatal(err)
	}
	o, err := demux.Open(context.Background(), filepath.Join(dir, "output.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { o.Close() })
	return dir, r, o
}

func TestSummaryContainsAllRequiredFields(t *testing.T) {
	dir, in, out := build(t)
	s, err := Run(context.Background(), in, out, dir)
	if err != nil {
		t.Fatal(err)
	}
	if s.Tracks != 3 || s.Samples != 36 || !s.SampleSame {
		t.Fatalf("summary=%+v", s)
	}
	if len(s.InputSHA) != 64 || len(s.OutputSHA) != 64 {
		t.Fatalf("digest lengths: %q %q", s.InputSHA, s.OutputSHA)
	}
	if len(s.Track) != 3 {
		t.Fatalf("tracks=%d", len(s.Track))
	}
	for _, tr := range s.Track {
		if !tr.Match || tr.InputHash != tr.OutputHash || len(tr.InputHash) != 64 {
			t.Fatalf("track summary=%+v", tr)
		}
		if tr.TargetID == 0 || tr.Samples != 12 {
			t.Fatalf("track summary=%+v", tr)
		}
		if tr.LastMicro < tr.FirstMicro {
			t.Fatalf("time range inverted: %+v", tr)
		}
	}
}

func TestSummaryText(t *testing.T) {
	dir, in, out := build(t)
	s, err := Run(context.Background(), in, out, dir)
	if err != nil {
		t.Fatal(err)
	}
	txt := s.Text()
	for _, want := range []string{"tracks=3", "samples=36", "input_sha256=", "output_sha256=",
		"sample_sha256_match=true", "track input=1"} {
		if !strings.Contains(txt, want) {
			t.Fatalf("text missing %q:\n%s", want, txt)
		}
	}
	if !strings.Contains(s.JSON(), `"track_summary"`) {
		t.Fatal("json missing track summary")
	}
}
