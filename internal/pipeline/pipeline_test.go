package pipeline

import (
	"context"
	"testing"

	"bcastqc/internal/fixture"
	"bcastqc/internal/media"
	"bcastqc/internal/mux"
)

const queue4MiB = 4 << 20

func runSuite(t *testing.T, suite string) Result {
	t.Helper()
	spec := fixture.SpecFor(suite, 42, queue4MiB)
	results, err := Run(context.Background(), []fixture.Spec{spec})
	if err != nil {
		t.Fatalf("run %s: %v", suite, err)
	}
	return results[0]
}

func TestCleanSucceedsAndProducesOutput(t *testing.T) {
	res := runSuite(t, fixture.SuiteClean)
	if res.Failed() {
		t.Fatalf("clean case failed: %v", res.Err)
	}
	if res.Metrics.OutputSHA256 == "" || res.Output == nil {
		t.Fatal("clean case must publish output and hash")
	}
	if res.Metrics.Lost != 0 || res.Metrics.RecoveryPct != 100 {
		t.Fatalf("clean traffic should have no loss: %+v", res.Metrics)
	}
}

func TestTruncateRejectsAndReportsOffset(t *testing.T) {
	res := runSuite(t, fixture.SuiteTruncate)
	if !res.Metrics.Rejected || res.Output != nil {
		t.Fatalf("truncated case must be rejected without output: %+v", res.Metrics)
	}
	if res.Metrics.FirstMissingOff < 0 {
		t.Fatalf("must report first missing offset: %+v", res.Metrics)
	}
	if !media.IsKind(res.Err, media.KindTruncated) {
		t.Fatalf("want truncated kind, got %v", res.Err)
	}
}

func TestSingleFileFailureDoesNotBlockBatch(t *testing.T) {
	specs := []fixture.Spec{
		fixture.SpecFor(fixture.SuiteTruncate, 42, queue4MiB),
		fixture.SpecFor(fixture.SuiteClean, 42, queue4MiB),
		fixture.SpecFor(fixture.SuiteRecovery, 42, queue4MiB),
	}
	results, err := Run(context.Background(), specs)
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("want 3 results, got %d", len(results))
	}
	if !results[0].Metrics.Rejected {
		t.Fatal("first case should be rejected")
	}
	if results[1].Failed() || results[2].Failed() {
		t.Fatal("subsequent cases must not be blocked by earlier failure")
	}
}

func TestDeterministicStatsAndHash(t *testing.T) {
	spec := fixture.SpecFor(fixture.SuiteRecovery, 42, queue4MiB)
	run := func() Result {
		results, err := Run(context.Background(), []fixture.Spec{spec})
		if err != nil {
			t.Fatal(err)
		}
		return results[0]
	}
	a, b := run(), run()
	if a.Metrics != b.Metrics {
		t.Fatalf("nondeterministic metrics:\n%+v\n%+v", a.Metrics, b.Metrics)
	}
}

func TestOutputVerifiesIndependently(t *testing.T) {
	res := runSuite(t, fixture.SuiteClean)
	vr, err := mux.Verify(res.Output, mustBlocks(t, res))
	if err != nil {
		t.Fatalf("independent verify: %v", err)
	}
	if !vr.PayloadOK || !vr.Monotonic || vr.BlockCount == 0 {
		t.Fatalf("verify flags wrong: %+v", vr)
	}
}

func TestBatchSizeGuarded(t *testing.T) {
	var many []fixture.Spec
	for i := 0; i < 33; i++ {
		many = append(many, fixture.SpecFor(fixture.SuiteClean, 1, queue4MiB))
	}
	if _, err := Run(context.Background(), many); err == nil {
		t.Fatal("batch of 33 must be rejected")
	}
}

// mustBlocks 从再封装输出回读块，供独立核对。
func mustBlocks(t *testing.T, res Result) []mux.MuxBlock {
	t.Helper()
	doc, err := parseOutput(res.Output)
	if err != nil {
		t.Fatal(err)
	}
	blocks := make([]mux.MuxBlock, 0, len(doc.Blocks))
	for _, b := range doc.Blocks {
		blocks = append(blocks, mux.MuxBlock{
			Track: b.Track, TimeNS: b.TimeNS, Key: b.Keyframe, Payload: b.Payload,
		})
	}
	return blocks
}
