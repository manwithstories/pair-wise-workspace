package engine_test

import (
	"context"
	"os"
	"reflect"
	"testing"

	"bcast/internal/engine"
	"bcast/internal/fault"
	"bcast/internal/fixture"
	"bcast/internal/index"
)

func setupRun(t *testing.T, dir string, interrupt int) (*engine.Report, *engine.Checkpoint, error) {
	t.Helper()
	set, err := fixture.Generate(dir, 41)
	if err != nil {
		t.Fatal(err)
	}
	scan, err := index.ScanInputs(set.Root, set.InputRels)
	if err != nil {
		t.Fatal(err)
	}
	if len(scan.Rejected) != 1 {
		t.Fatalf("rejected=%d want 1", len(scan.Rejected))
	}
	params := engine.DefaultParams()
	key, err := index.NewKey(scan.InputHashes(), set.Timeline, params)
	if err != nil {
		t.Fatal(err)
	}
	store := &index.Store{Key: key, Assets: scan.Store.Assets}
	if err := store.Save(set.Root, "state/index.json"); err != nil {
		t.Fatal(err)
	}
	return engine.Run(context.Background(), engine.Config{
		Root: set.Root, Timeline: set.Timeline, Key: key, Index: store,
		Params: params, InterruptAfter: interrupt,
	})
}

func TestInterruptResumeMatchesCleanRun(t *testing.T) {
	interruptDir := t.TempDir()
	rep, cp, err := setupRun(t, interruptDir, 2)
	if err == nil || !fault.IsCode(err, fault.Interrupted) {
		t.Fatalf("expected interrupt 75, got err=%v", err)
	}
	if fault.ExitCode(err) != 75 {
		t.Fatalf("exit=%d want 75", fault.ExitCode(err))
	}
	if rep.Processed != 2 || rep.Resumed != 0 {
		t.Fatalf("first run processed=%d resumed=%d", rep.Processed, rep.Resumed)
	}
	if cp.LastCompleted != 1 || len(cp.SegmentSHA256) != 2 {
		t.Fatalf("checkpoint last=%d hashes=%d", cp.LastCompleted, len(cp.SegmentSHA256))
	}

	// Resume the same root with no simulated interruption.
	set, err := fixture.Generate(interruptDir, 41)
	if err != nil {
		t.Fatal(err)
	}
	scan, err := index.ScanInputs(set.Root, set.InputRels)
	if err != nil {
		t.Fatal(err)
	}
	params := engine.DefaultParams()
	key, err := index.NewKey(scan.InputHashes(), set.Timeline, params)
	if err != nil {
		t.Fatal(err)
	}
	resumed, cp2, err := engine.Run(context.Background(), engine.Config{
		Root: set.Root, Timeline: set.Timeline, Key: key, Index: scan.Store, Params: params,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Resumed != 2 || resumed.Processed != 6 {
		t.Fatalf("resumed=%d processed=%d want 2/6", resumed.Resumed, resumed.Processed)
	}
	if len(resumed.Hashes) != 8 || cp2.LastCompleted != 7 {
		t.Fatalf("hashes=%d last=%d", len(resumed.Hashes), cp2.LastCompleted)
	}

	cleanDir := t.TempDir()
	clean, _, err := setupRun(t, cleanDir, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(clean.Hashes) != 8 {
		t.Fatalf("clean hashes=%d want 8", len(clean.Hashes))
	}
	if !reflect.DeepEqual(resumed.Hashes, clean.Hashes) {
		t.Fatalf("resume/clean segment hashes differ\nresume=%v\nclean =%v", resumed.Hashes, clean.Hashes)
	}
}

func TestFailedSegmentDoesNotAdvanceCheckpoint(t *testing.T) {
	dir := t.TempDir()
	set, err := fixture.Generate(dir, 41)
	if err != nil {
		t.Fatal(err)
	}
	// Build the key while all inputs exist, then remove B to simulate a bad segment.
	scan, err := index.ScanInputs(set.Root, set.InputRels)
	if err != nil {
		t.Fatal(err)
	}
	key, err := index.NewKey(scan.InputHashes(), set.Timeline, engine.DefaultParams())
	if err != nil {
		t.Fatal(err)
	}
	bPath, err := set.Root.Resolve("assets/b.mp4")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(bPath); err != nil {
		t.Fatal(err)
	}
	_, cp, err := engine.Run(context.Background(), engine.Config{
		Root: set.Root, Timeline: set.Timeline, Key: key, Index: scan.Store, Params: engine.DefaultParams(),
	})
	if err == nil {
		t.Fatal("missing source must fail segment")
	}
	if cp == nil || cp.LastCompleted != 2 {
		t.Fatalf("last completed=%v want 2", cp)
	}
	if _, err := engine.LoadManifest(set.Root, 3); err == nil {
		t.Fatal("failed segment must not leave a committable manifest")
	}
	if _, err := engine.LoadManifest(set.Root, 0); err != nil {
		t.Fatalf("previously committed segment must remain valid: %v", err)
	}
}
