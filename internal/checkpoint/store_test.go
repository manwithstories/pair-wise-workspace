package checkpoint

import (
	"os"
	"testing"

	"github.com/mediamend/mediamend/internal/model"
)

func TestSaveLoadAndSkip(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(dir)
	cp := Fresh("digest-A")
	if err := st.Mark(cp, StageIngest, Position{FrameSeq: 100}); err != nil {
		t.Fatal(err)
	}
	if err := st.Mark(cp, StageTimeline, Position{FrameSeq: 100}); err != nil {
		t.Fatal(err)
	}
	got, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !got.Skip(StageIngest) || got.Skip(StageIndex) {
		t.Fatalf("skip flags wrong: %+v", got.Completed)
	}
	skipped := got.SkippedStages()
	if len(skipped) != 2 || skipped[0] != StageIngest || skipped[1] != StageTimeline {
		t.Fatalf("skipped=%v", skipped)
	}
}

func TestLoadMissingReturnsNil(t *testing.T) {
	st := NewStore(t.TempDir())
	cp, err := st.Load()
	if err != nil || cp != nil {
		t.Fatalf("missing checkpoint should be nil,nil got %v %v", cp, err)
	}
}

func TestResumeDigestMismatchRejected(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(dir)
	cp := Fresh("digest-A")
	if err := st.Mark(cp, StageIngest, Position{}); err != nil {
		t.Fatal(err)
	}
	got, faults, err := st.Resume("digest-B")
	if got != nil || err == nil {
		t.Fatal("mismatched resume must be rejected")
	}
	if len(faults) != 1 || faults[0].Class != model.FaultCheckpoint || faults[0].Recoverable() {
		t.Fatalf("faults=%v", faults)
	}
	if code := model.ExitCode(faults); code != 4 {
		t.Fatalf("exit=%d want 4", code)
	}
}

func TestResumeMatchingDigestReuses(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(dir)
	cp := Fresh("digest-A")
	if err := st.Mark(cp, StageColor, Position{TrackID: "v1"}); err != nil {
		t.Fatal(err)
	}
	got, faults, err := st.Resume("digest-A")
	if err != nil || faults != nil || got == nil {
		t.Fatalf("valid resume failed: %v %v", err, faults)
	}
	if p := got.Positions[StageColor]; p.TrackID != "v1" {
		t.Fatalf("position lost: %+v", p)
	}
}

func TestAtomicWriteLeavesNoTempFile(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(dir)
	if err := st.Save(Fresh("d")); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Load(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() == FileName+".tmp" {
			t.Fatal("temp file left behind after atomic save")
		}
	}
}

func TestMetricAndOutputSHARoundTrip(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(dir)
	cp := Fresh("digest-X")
	cp.SetMetric(MetricFramesTotal, 10000)
	cp.SetMetric(MetricSeekChecked, 800)
	cp.SetMetric(MetricCorruptLocated, 100)
	cp.OutputSHA = "abc123"
	if err := st.Save(cp); err != nil {
		t.Fatal(err)
	}
	got, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.Metric(MetricFramesTotal) != 10000 || got.Metric(MetricSeekChecked) != 800 ||
		got.Metric(MetricCorruptLocated) != 100 {
		t.Fatalf("metrics lost: %+v", got.Metrics)
	}
	if got.OutputSHA != "abc123" {
		t.Fatalf("output sha lost: %q", got.OutputSHA)
	}
}
