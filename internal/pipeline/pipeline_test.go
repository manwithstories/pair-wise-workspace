package pipeline

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mediascan/internal/fixture"
	"mediascan/internal/index"
	"mediascan/internal/manifest"
	"mediascan/internal/model"
	"mediascan/internal/recover"
)

// prepare generates a full demo workspace and returns its options.
func prepare(t *testing.T) Options {
	t.Helper()
	work := t.TempDir()
	l, err := fixture.Generate(work)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.GenerateRTP(l.RTPFile, 240, 17); err != nil {
		t.Fatal(err)
	}
	return Options{Work: work, LossEvery: 17, RTPPackets: 240}
}

func runFull(t *testing.T, o Options) *Result {
	t.Helper()
	res, err := Run(o, 0)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return res
}

func TestRunReportsRequiredConclusions(t *testing.T) {
	o := prepare(t)
	res := runFull(t, o)
	if res.Segments != fixture.SegmentCount {
		t.Fatalf("segments=%d, want %d", res.Segments, fixture.SegmentCount)
	}
	if len(res.CorruptRange) < 1 {
		t.Fatalf("corrupt_ranges=%d, want >=1", len(res.CorruptRange))
	}
	if res.AVSync.MaxErrorMS > 40 || res.AVSync.MaxErrorMS < 1 {
		t.Fatalf("av_sync_max_ms=%d, want (0,40]", res.AVSync.MaxErrorMS)
	}
	if res.RTP.Dropped < 1 {
		t.Fatalf("packets_lost=%d", res.RTP.Dropped)
	}
	if res.RTP.RecoveryPct <= 0 || res.RTP.RecoveryPct >= 100 {
		t.Fatalf("loss_recovery_pct=%v", res.RTP.RecoveryPct)
	}
	if res.InputSHA == "" || res.ResultSHA == "" {
		t.Fatal("missing digests")
	}
	if res.FirstMiss == "" {
		t.Fatal("first missing dependency not reported")
	}
	// Missing media and timestamp anomalies must reach the shared fault list.
	counts := map[model.FaultKind]int{}
	for _, f := range res.Faults {
		counts[f.Kind]++
		if err := f.Validate(); err != nil {
			t.Fatalf("invalid fault: %v", err)
		}
	}
	if counts[model.FaultMissingSegment] == 0 {
		t.Error("missing_segment fault not propagated")
	}
	if counts[model.FaultTimestamp] == 0 && res.AVSync.ClockSkews == 0 {
		t.Error("timestamp anomaly not propagated")
	}
	if counts[model.FaultKeyframe] == 0 && res.Keyframes.Irregular == 0 {
		t.Error("keyframe fault not propagated")
	}
}

func TestMissingAndTimestampReachSyncAndProvenance(t *testing.T) {
	o := prepare(t)
	res := runFull(t, o)
	// The A/V phase must have attributed the maximum skew to a real segment.
	if res.AVSync.MaxErrorSeg == "" {
		t.Fatal("max sync error has no segment attribution")
	}
	provPath := filepath.Join(o.Work, "report", "provenance.jsonl")
	data, err := os.ReadFile(provPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, want := range []string{"missing_segment", "input_sha256", "result_sha256", "checkpoint_next_segment"} {
		if !contains(text, want) {
			t.Errorf("provenance missing %q", want)
		}
	}
	if !contains(text, res.AVSync.MaxErrorSeg) && res.AVSync.MaxErrorMS > 40 {
		t.Error("sync fault segment not in provenance")
	}
}

func TestRerunProducesIdenticalResult(t *testing.T) {
	o := prepare(t)
	first := runFull(t, o)
	second := runFull(t, o)
	if first.ResultSHA != second.ResultSHA {
		t.Fatalf("rerun digest changed: %s != %s", first.ResultSHA, second.ResultSHA)
	}
	if len(first.Faults) != len(second.Faults) {
		t.Fatalf("fault count changed: %d != %d", len(first.Faults), len(second.Faults))
	}
}

func TestInterruptThenResumeMatchesFullRun(t *testing.T) {
	o := prepare(t)
	full := runFull(t, o)
	// Use a fresh directory so the first run's state cannot leak in.
	o2 := prepare(t)
	partial, err := Run(o2, 3)
	if !errors.Is(err, ErrInterrupted) {
		t.Fatalf("expected interruption, got %v", err)
	}
	if partial.ResumeReused != 0 {
		t.Fatalf("fresh run reused %d segments", partial.ResumeReused)
	}
	if !partial.Interrupted {
		t.Fatal("result not marked interrupted")
	}
	resumed, err := Run(o2, 0)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.ResumeReused != 3 {
		t.Fatalf("resume_reused_segments=%d, want 3", resumed.ResumeReused)
	}
	if resumed.ResultSHA != full.ResultSHA {
		t.Fatalf("resume digest %s != full digest %s", resumed.ResultSHA, full.ResultSHA)
	}
}

func TestInputChangeInvalidatesCheckpoint(t *testing.T) {
	o := prepare(t)
	runFull(t, o)
	// Delete media and verify rejects: the checkpoint was bound to the old
	// input digest.
	victim := fixture.SegmentPath(o.Work, 0)
	if err := os.Remove(victim); err != nil {
		t.Fatal(err)
	}
	// Verify must reject the checkpoint recorded for the old input state.
	code, msg := Verify(o)
	if code != VerifyMismatch {
		t.Fatalf("verify code=%d msg=%s, want %d", code, msg, VerifyMismatch)
	}
	// A fresh run must not reuse any entry from the stale checkpoint.
	res, err := Run(o, 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.ResumeReused != 0 {
		t.Fatalf("stale checkpoint reused %d segments after input change", res.ResumeReused)
	}
}

func TestVerifyDetectsTamperedResult(t *testing.T) {
	o := prepare(t)
	runFull(t, o)
	if code, msg := Verify(o); code != VerifyOK {
		t.Fatalf("clean verify failed: %d %s", code, msg)
	}
	store := recover.NewStore(o.Work)
	cp, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	cp.ResultSHA = "0000000000000000000000000000000000000000000000000000000000000000"
	if err := store.Save(cp); err != nil {
		t.Fatal(err)
	}
	if code, _ := Verify(o); code != VerifyMismatch {
		t.Fatalf("tampered checkpoint accepted: code=%d", code)
	}
}

func TestVerifyDetectsProvenanceMismatch(t *testing.T) {
	o := prepare(t)
	runFull(t, o)
	provPath := filepath.Join(o.Work, "report", "provenance.jsonl")
	data, err := os.ReadFile(provPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(provPath, append([]byte(`{"kind":"summary","input_sha256":"deadbeef","result_sha256":"cafe"}`+"\n"), data...), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _ := Verify(o); code != VerifyMismatch {
		t.Fatalf("provenance mismatch accepted: code=%d", code)
	}
}

func TestVerifyUsesCheckpointParameters(t *testing.T) {
	o := prepare(t)
	runFull(t, o)
	// Simulate verify being run with different CLI defaults: the checkpoint's
	// bound parameters must win, otherwise a legitimate rerun looks tampered.
	alt := o
	alt.AVTolMS = 5
	alt.MaxGOPMS = 100
	if code, msg := Verify(alt); code != VerifyOK {
		t.Fatalf("verify with differing defaults: code=%d msg=%s", code, msg)
	}
}

func TestVerifyMissingCheckpointIsMismatch(t *testing.T) {
	o := prepare(t)
	if code, _ := Verify(o); code == VerifyOK {
		t.Fatal("verify without a prior run should not pass")
	}
}

func TestScanIndexBoundToManifest(t *testing.T) {
	work := t.TempDir()
	l, err := fixture.Generate(work)
	if err != nil {
		t.Fatal(err)
	}
	_ = l
	res, err := manifest.NewResolver(work)
	if err != nil {
		t.Fatal(err)
	}
	tl, err := manifest.Load(res, "hls/media.m3u8")
	if err != nil {
		t.Fatal(err)
	}
	manifest.MarkPresence(res, tl)
	idx, err := index.Scan(res, tl, index.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !idx.Complete || len(idx.Entries) != fixture.SegmentCount {
		t.Fatalf("index incomplete: %+v", idx)
	}
	if idx.FirstMiss == "" {
		t.Fatal("first missing not recorded")
	}
	// Rescanning the same inputs must yield the same input digest.
	idx2, err := index.Scan(res, tl, index.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if idx.InputSHA != idx2.InputSHA {
		t.Fatal("input digest unstable across scans")
	}
}

func contains(haystack, needle string) bool { return strings.Contains(haystack, needle) }
