package pipeline

import (
	"testing"

	"archivecheck/internal/fault"
	"archivecheck/internal/scheduler"
)

func testParams(inject string) Params {
	return Params{
		DurationSec:  60,
		BitratesKbps: []int{800, 1800, 3500},
		Inject:       inject,
		ColorPolicy:  "keep-source",
		SegmentSec:   2,
	}
}

func runOnce(t *testing.T, p Params, dir, stopAfter string) (*scheduler.Runner, *scheduler.Report) {
	t.Helper()
	r := scheduler.New(p.Hash())
	if err := Build(r, p, dir); err != nil {
		t.Fatal(err)
	}
	rep, err := r.Run(stopAfter)
	if err != nil {
		t.Fatal(err)
	}
	return r, rep
}

func finalOf(t *testing.T, r *scheduler.Runner) FinalOut {
	t.Helper()
	var fo FinalOut
	if err := r.Output("checksum", &fo); err != nil {
		t.Fatal(err)
	}
	return fo
}

func TestNormalPath(t *testing.T) {
	p := testParams("")
	r, rep := runOnce(t, p, t.TempDir(), "")
	if !rep.Completed || rep.RerunNodes != 4 {
		t.Fatalf("report = %+v", rep)
	}
	fo := finalOf(t, r)
	if fo.Segments != 30 || fo.Representations != 3 {
		t.Fatalf("segments=%d representations=%d", fo.Segments, fo.Representations)
	}
	if fo.MaxBoundarySkewMs > 40 {
		t.Fatalf("skew = %d ms", fo.MaxBoundarySkewMs)
	}
	if len(fo.Checksum) != 64 {
		t.Fatalf("checksum = %q", fo.Checksum)
	}
	if len(fo.Faults) != 0 {
		t.Fatalf("faults = %v", fo.Faults)
	}
	// Manifest coverage: segments tile the whole 60 s timeline.
	var seg SegOut
	if err := r.Output("segment", &seg); err != nil {
		t.Fatal(err)
	}
	first := seg.Manifest.Segments[0]
	last := seg.Manifest.Segments[len(seg.Manifest.Segments)-1]
	if first.StartMs != 0 || last.EndMs < 60000 {
		t.Fatalf("coverage = [%d,%d) ms", first.StartMs, last.EndMs)
	}
}

func TestCorruptPathLocated(t *testing.T) {
	p := testParams("truncate:track2:sample1450")
	r, rep := runOnce(t, p, t.TempDir(), "")
	if !rep.Completed {
		t.Fatalf("recoverable corruption must not stop the run: %+v", rep)
	}
	fo := finalOf(t, r)
	fl := fo.Faults.First()
	if fl == nil || fl.Kind != fault.KindTruncatedSampleTable {
		t.Fatalf("faults = %v", fo.Faults)
	}
	if fl.Track != 2 || fl.Sample != 1450 {
		t.Fatalf("fault at track=%d sample=%d, want 2/1450", fl.Track, fl.Sample)
	}
	if fo.Faults.FirstBlockedName() != "normalize" {
		t.Fatalf("first blocked = %q", fo.Faults.FirstBlockedName())
	}
	// Recoverable range of track 2 stops at sample 1449.
	var scan ScanOut
	if err := r.Output("scan", &scan); err != nil {
		t.Fatal(err)
	}
	if scan.Reps[0].Audio.Valid != 1449 {
		t.Fatalf("audio valid = %d, want 1449", scan.Reps[0].Audio.Valid)
	}
}

func TestResumeMatchesFullRun(t *testing.T) {
	p := testParams("truncate:track2:sample1450")

	full, rep := runOnce(t, p, t.TempDir(), "")
	if !rep.Completed {
		t.Fatal("full run did not complete")
	}
	want := finalOf(t, full).Checksum

	dir := t.TempDir()
	interrupted, rep := runOnce(t, p, dir, "normalize")
	if !rep.Interrupted || rep.InterruptedAfter != "normalize" {
		t.Fatalf("interrupt = %+v", rep)
	}
	if err := interrupted.SaveState(dir); err != nil {
		t.Fatal(err)
	}

	resumed, err := scheduler.LoadState(dir, p.Hash())
	if err != nil {
		t.Fatal(err)
	}
	if err := Build(resumed, p, dir); err != nil {
		t.Fatal(err)
	}
	rep, err = resumed.Run("")
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Completed || rep.RerunNodes != 2 || rep.SkippedNodes != 2 {
		t.Fatalf("resume = %+v", rep)
	}
	if got := finalOf(t, resumed).Checksum; got != want {
		t.Fatalf("resumed checksum %s != full-run %s", got, want)
	}
}

func TestIdempotentRerun(t *testing.T) {
	p := testParams("")
	dir := t.TempDir()
	r, _ := runOnce(t, p, dir, "")
	want := finalOf(t, r).Checksum

	// Same runner: nothing reruns.
	rep, err := r.Run("")
	if err != nil {
		t.Fatal(err)
	}
	if rep.RerunNodes != 0 || rep.SkippedNodes != 4 {
		t.Fatalf("second run = %+v", rep)
	}

	// Fresh runner, same params and inputs: identical checksum.
	r2, _ := runOnce(t, p, dir, "")
	if got := finalOf(t, r2).Checksum; got != want {
		t.Fatalf("checksum %s != %s across identical runs", got, want)
	}
}
