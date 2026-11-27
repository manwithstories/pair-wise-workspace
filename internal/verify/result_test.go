package verify

import (
	"testing"

	"relayaudit/internal/fault"
	"relayaudit/internal/fixture"
	"relayaudit/internal/timeline"
	"relayaudit/internal/transform"
	"relayaudit/internal/wire"
)

// runRepair is a compact end-to-end helper mirroring the pipeline's stages.
func runRepair(t *testing.T, spec fixture.Spec) (*fixture.Segment, *wire.ScanResult, []transform.Range) {
	t.Helper()
	seg, err := fixture.Generate(spec)
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	scan, err := wire.Scan(seg.Bytes)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	plan := timeline.SamplePlan(scan.Records)
	_, bf, err := timeline.Resolve(scan.Records, plan)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	ranges := transform.BuildRanges(append(append([]*fault.Fault{}, scan.Damage...), bf...), scan.SourceSize)
	return seg, scan, ranges
}

func TestSHA256Hex(t *testing.T) {
	if got := SHA256Hex([]byte("abc")); got != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Fatalf("digest = %s", got)
	}
}

func TestCheckAcceptsCleanRepair(t *testing.T) {
	seg, scan, ranges := runRepair(t, fixture.Spec{Seed: 73, Size: 1 << 20})
	e := transform.NewEncoder()
	for _, rec := range scan.Records {
		if _, hit := transform.Overlaps(rec, ranges); hit {
			continue
		}
		e.Add(rec)
	}
	out, err := e.Finalize(uint8(scan.Header.Tracks))
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}
	res, _, _ := timeline.Resolve(scan.Records, timeline.SamplePlan(scan.Records))
	exp := Expected{
		DamageRanges: 0, TrackCount: 4, SourceSize: int64(len(seg.Bytes)),
	}
	got := Check(out, scan, res, ranges, SHA256Hex(out), exp)
	if len(got.Problems) != 0 {
		t.Fatalf("problems: %v", got.Problems)
	}
	if got.ExitCode != 0 {
		t.Fatalf("exit code = %d", got.ExitCode)
	}
	if got.RepeatSHA256Equal != 1 {
		t.Fatalf("repeat digest not equal")
	}
	if len(got.OutputSHA256) != 64 {
		t.Fatalf("digest length = %d", len(got.OutputSHA256))
	}
	if len(got.Tracks) != 4 {
		t.Fatalf("tracks = %v", got.Tracks)
	}
	if got.KeyIntervalTicks[1] != 2*90000 {
		t.Fatalf("key interval = %d", got.KeyIntervalTicks[1])
	}
}

func TestCheckFailsOnDigestMismatch(t *testing.T) {
	seg, scan, ranges := runRepair(t, fixture.Spec{Seed: 1, Size: 1 << 17})
	e := transform.NewEncoder()
	for _, rec := range scan.Records {
		if _, hit := transform.Overlaps(rec, ranges); !hit {
			e.Add(rec)
		}
	}
	out, _ := e.Finalize(4)
	res, _, _ := timeline.Resolve(scan.Records, timeline.SamplePlan(scan.Records))
	got := Check(out, scan, res, ranges, "deadbeef", Expected{DamageRanges: 0, TrackCount: 4, SourceSize: int64(len(seg.Bytes))})
	if got.ExitCode != 4 {
		t.Fatalf("exit code = %d, want 4", got.ExitCode)
	}
	if got.RepeatSHA256Equal != 0 {
		t.Fatalf("mismatch reported equal")
	}
}

func TestCheckFailsOnUnparseableOutput(t *testing.T) {
	seg, scan, ranges := runRepair(t, fixture.Spec{Seed: 2, Size: 1 << 17})
	res, _, _ := timeline.Resolve(scan.Records, timeline.SamplePlan(scan.Records))
	got := Check([]byte("garbage"), scan, res, ranges, "", Expected{
		DamageRanges: 0, TrackCount: 4, SourceSize: int64(len(seg.Bytes)),
	})
	if got.ExitCode != 4 || len(got.Problems) == 0 {
		t.Fatalf("garbage output accepted: %+v", got)
	}
}

func TestCheckMeasuresDamageLocalisation(t *testing.T) {
	seg, scan, ranges := runRepair(t, fixture.Spec{
		Seed: 73, Size: 1 << 18, CorruptRecord: 17, TruncateTail: 41,
	})
	e := transform.NewEncoder()
	for _, rec := range scan.Records {
		if _, hit := transform.Overlaps(rec, ranges); !hit {
			e.Add(rec)
		}
	}
	out, _ := e.Finalize(4)
	res, _, _ := timeline.Resolve(scan.Records, timeline.SamplePlan(scan.Records))
	exp := Expected{
		CorruptOffset:   seg.Damage.CorruptRecordOff,
		CorruptInjected: true,
		TruncateTail:    seg.Damage.TruncateTail,
		DamageRanges:    2,
		TrackCount:      4,
		SourceSize:      int64(len(seg.Bytes)),
	}
	got := Check(out, scan, res, ranges, SHA256Hex(out), exp)
	if len(got.Problems) != 0 {
		t.Fatalf("problems: %v", got.Problems)
	}
	if got.CorruptOffsetDelta != 0 {
		t.Fatalf("corrupt delta = %d", got.CorruptOffsetDelta)
	}
	if got.TruncatedTailBytes != 41 {
		t.Fatalf("truncated tail = %d", got.TruncatedTailBytes)
	}
	if got.DamageRanges != 2 {
		t.Fatalf("damage ranges = %d", got.DamageRanges)
	}
}

func TestCheckFlagsWrongDamageCount(t *testing.T) {
	seg, scan, ranges := runRepair(t, fixture.Spec{Seed: 5, Size: 1 << 17, CorruptRecord: 9})
	e := transform.NewEncoder()
	for _, rec := range scan.Records {
		if _, hit := transform.Overlaps(rec, ranges); !hit {
			e.Add(rec)
		}
	}
	out, _ := e.Finalize(4)
	res, _, _ := timeline.Resolve(scan.Records, timeline.SamplePlan(scan.Records))
	got := Check(out, scan, res, ranges, "", Expected{
		DamageRanges: 7, TrackCount: 4, SourceSize: int64(len(seg.Bytes)),
	})
	if got.ExitCode != 4 {
		t.Fatalf("wrong damage count accepted")
	}
}

func TestCheckFlagsMissingLocalisation(t *testing.T) {
	seg, scan, ranges := runRepair(t, fixture.Spec{Seed: 6, Size: 1 << 17})
	e := transform.NewEncoder()
	for _, rec := range scan.Records {
		e.Add(rec)
	}
	out, _ := e.Finalize(4)
	res, _, _ := timeline.Resolve(scan.Records, timeline.SamplePlan(scan.Records))
	got := Check(out, scan, res, ranges, "", Expected{
		CorruptOffset: 1234, CorruptInjected: true, TruncateTail: 41,
		DamageRanges: 0, TrackCount: 4, SourceSize: int64(len(seg.Bytes)),
	})
	if got.ExitCode != 4 {
		t.Fatalf("missing localisation not flagged")
	}
	foundCRC, foundTail := false, false
	for _, p := range got.Problems {
		if contains(p, "not localised") {
			foundCRC = true
		}
		if contains(p, "not isolated") {
			foundTail = true
		}
	}
	if !foundCRC || !foundTail {
		t.Fatalf("problems = %v", got.Problems)
	}
}

func TestCheckFlagsTrackCountMismatch(t *testing.T) {
	seg, scan, ranges := runRepair(t, fixture.Spec{Seed: 7, Size: 1 << 16})
	e := transform.NewEncoder()
	for _, rec := range scan.Records {
		e.Add(rec)
	}
	out, _ := e.Finalize(2)
	res, _, _ := timeline.Resolve(scan.Records, timeline.SamplePlan(scan.Records))
	got := Check(out, scan, res, ranges, "", Expected{
		DamageRanges: 0, TrackCount: 4, SourceSize: int64(len(seg.Bytes)),
	})
	if got.ExitCode != 4 {
		t.Fatalf("track count mismatch not flagged")
	}
}

func TestExitCodeForProblems(t *testing.T) {
	if exitCodeFor(nil) != 0 {
		t.Fatalf("clean result exited non-zero")
	}
	if exitCodeFor([]string{"x"}) != 4 {
		t.Fatalf("problem exited without 4")
	}
	if abs64(-9) != 9 || abs64(3) != 3 {
		t.Fatalf("abs64 wrong")
	}
	if boolToInt(true) != 1 || boolToInt(false) != 0 {
		t.Fatalf("boolToInt wrong")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
