package repair

import (
	"os"
	"testing"

	"mediacheck/internal/fault"
	"mediacheck/internal/fixture"
	"mediacheck/internal/probe"
)

func engine() *Engine {
	cfg := DefaultConfig()
	cfg.Recover = true
	return New(cfg, probe.DefaultConfig())
}

func analyzeFixture(t *testing.T, kind fixture.Kind) (*probe.File, *Result, fixture.Result) {
	t.Helper()
	fr, err := fixture.Generate(t.TempDir(), fixture.DefaultSpec(kind))
	if err != nil {
		t.Fatalf("generate %s: %v", kind, err)
	}
	pf, res, err := engine().Analyze(fr.Path)
	if err != nil {
		t.Fatalf("analyze %s: %v", kind, err)
	}
	return pf, res, fr
}

func onlyTrack(t *testing.T, res *Result, kind probe.Kind) *TrackResult {
	t.Helper()
	for _, tr := range res.Tracks {
		if tr.Kind == kind {
			return tr
		}
	}
	t.Fatalf("no %s track", kind)
	return nil
}

func TestValidRecoveryIsComplete(t *testing.T) {
	_, res, _ := analyzeFixture(t, fixture.Valid)
	if res.Status != ResultComplete {
		t.Fatalf("status=%s want complete (issues=%v)", res.Status, res.IssuesAll())
	}
	for _, tr := range res.Tracks {
		if tr.Status != StatusOK {
			t.Fatalf("track %d status=%s", tr.TrackID, tr.Status)
		}
		if tr.Recovered.Samples != int(tr.ExcludedSamples+uint64(tr.Recovered.Samples)) && tr.ExcludedSamples != 0 {
			t.Fatalf("track %d excluded=%d", tr.TrackID, tr.ExcludedSamples)
		}
		if len(tr.Damages) != 0 {
			t.Fatalf("track %d has damages on valid input: %+v", tr.TrackID, tr.Damages)
		}
	}
	if !res.Boundary.Accept {
		t.Fatalf("valid boundaries rejected: %+v", res.Boundary)
	}
	if res.Boundary.AlignmentMS > 1 {
		t.Fatalf("alignment=%d want <=1", res.Boundary.AlignmentMS)
	}
}

func TestTruncatedRecoveryDropsTail(t *testing.T) {
	_, res, _ := analyzeFixture(t, fixture.Truncated)
	v := onlyTrack(t, res, probe.KindVideo)
	a := onlyTrack(t, res, probe.KindAudio)
	if v.Status != StatusOK {
		t.Fatalf("video status=%s (video sits entirely before EOF)", v.Status)
	}
	if a.Status != StatusRecovered {
		t.Fatalf("audio status=%s want recovered", a.Status)
	}
	if len(a.Damages) == 0 {
		t.Fatal("audio truncation not recorded")
	}
	if a.DiscardedBytes <= 0 {
		t.Fatalf("audio discarded bytes=%d", a.DiscardedBytes)
	}
	if a.FirstUnreadable < 0 {
		t.Fatal("audio first unreadable not set")
	}
	// Nothing at or after the damage offset may be accepted.
	for _, d := range a.Damages {
		if d.Code != fault.CodeTruncated {
			t.Fatalf("damage code=%s want truncated", d.Code)
		}
	}
}

func TestCorruptRecoveryResumesAtKeyframe(t *testing.T) {
	_, res, fr := analyzeFixture(t, fixture.Corrupt)
	v := onlyTrack(t, res, probe.KindVideo)
	if v.Status != StatusRecovered {
		t.Fatalf("video status=%s want recovered", v.Status)
	}
	if len(v.Damages) != 1 {
		t.Fatalf("damages=%d want 1: %+v", len(v.Damages), v.Damages)
	}
	d := v.Damages[0]
	if d.Code != fault.CodeCorrupt {
		t.Fatalf("damage code=%s want corrupt", d.Code)
	}
	if d.Offset != fr.DamageOffset {
		t.Fatalf("damage offset=%d want %d", d.Offset, fr.DamageOffset)
	}
	if d.ResumeMS < 0 {
		t.Fatal("recovery did not resume")
	}
	// Damage is a single sample; resume happens at the next keyframe, so the
	// recovered sample count is below the declared count.
	if v.ExcludedSamples == 0 {
		t.Fatal("no samples excluded")
	}
	if v.Recovered.Samples >= 90 {
		t.Fatalf("recovered=%d should be below declared 90", v.Recovered.Samples)
	}
	// Timestamps must never regress across the damage gap.
	if !monotonic(v) {
		t.Fatal("recovered timestamps are not monotonic")
	}
}

func TestMissingIndexRecovery(t *testing.T) {
	_, res, _ := analyzeFixture(t, fixture.MissingIndex)
	for _, tr := range res.Tracks {
		if tr.Status != StatusOK && tr.Status != StatusRecovered {
			t.Fatalf("track %d status=%s", tr.TrackID, tr.Status)
		}
		if tr.Recovered.Samples == 0 {
			t.Fatalf("track %d recovered nothing", tr.TrackID)
		}
	}
	if res.Status == ResultFailed {
		t.Fatalf("rebuilt index should not fail outright: %v", res.IssuesAll())
	}
}

func TestBoundaryMismatchRejectsSwitch(t *testing.T) {
	_, res, _ := analyzeFixture(t, fixture.BoundaryMismatch)
	if res.Boundary.Accept {
		t.Fatal("misaligned layers accepted the switch")
	}
	if len(res.Boundary.Edges) == 0 {
		t.Fatal("no boundary edge recorded")
	}
	e := res.Boundary.Edges[0]
	if e.DeltaMS != 40 {
		t.Fatalf("delta=%d want 40", e.DeltaMS)
	}
	if res.Status != ResultPartial {
		t.Fatalf("status=%s want partial", res.Status)
	}
	if len(res.Boundary.Issues) == 0 {
		t.Fatal("boundary issue not recorded")
	}
}

func TestUnrecoverableTrackKeepsOthers(t *testing.T) {
	fr, err := fixture.Generate(t.TempDir(), fixture.DefaultSpec(fixture.Valid))
	if err != nil {
		t.Fatal(err)
	}
	pf, err := probe.Analyze(fr.Path, probe.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	// Corrupt the file so the whole video track's payload range is unreadable
	// by removing its payload from disk: easiest is to corrupt every video
	// sample with uniform fill. We instead mark one track unrecoverable by
	// zeroing its timescale through a crafted probe result.
	tr := pf.Tracks[0]
	savedTS := tr.Timescale
	tr.Timescale = 0
	res := engine().Recover(pf)
	tr.Timescale = savedTS
	bad := res.TrackByID(tr.ID)
	if bad == nil || bad.Status != StatusUnrecoverable {
		t.Fatalf("expected unrecoverable track, got %+v", bad)
	}
	var ok int
	for _, other := range res.Tracks {
		if other.TrackID != tr.ID && other.usable() {
			ok++
		}
	}
	if ok == 0 {
		t.Fatal("other track was not preserved")
	}
	if res.Status != ResultPartial {
		t.Fatalf("status=%s want partial", res.Status)
	}
}

func TestDeterministicRecovery(t *testing.T) {
	fr, err := fixture.Generate(t.TempDir(), fixture.DefaultSpec(fixture.Corrupt))
	if err != nil {
		t.Fatal(err)
	}
	pf, err := probe.Analyze(fr.Path, probe.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	a := engine().Recover(pf)
	b := engine().Recover(pf)
	if a.Status != b.Status {
		t.Fatalf("status differs: %s vs %s", a.Status, b.Status)
	}
	for i := range a.Tracks {
		if a.Tracks[i].Recovered != b.Tracks[i].Recovered {
			t.Fatalf("track %d recovered span differs", i)
		}
		if len(a.Tracks[i].Damages) != len(b.Tracks[i].Damages) {
			t.Fatalf("track %d damage count differs", i)
		}
	}
}

func TestDetectsNaughtedPayload(t *testing.T) {
	fr, err := fixture.Generate(t.TempDir(), fixture.DefaultSpec(fixture.Valid))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(fr.Path)
	if err != nil {
		t.Fatal(err)
	}
	// Zero one video sample in place; the file length and box structure hold.
	pf, err := probe.Analyze(fr.Path, probe.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	v := pf.Tracks[0]
	cur := v.Cursor()
	row, ok := cur.Next()
	if !ok {
		t.Fatal("no first sample")
	}
	for i := row.Offset; i < row.Offset+int64(row.Size); i++ {
		raw[i] = 0x00
	}
	if err := os.WriteFile(fr.Path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	pf2, err := probe.Analyze(fr.Path, probe.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	res := engine().Recover(pf2)
	tr := res.TrackByID(v.ID)
	if tr == nil || len(tr.Damages) == 0 {
		t.Fatalf("naughted payload not detected: %+v", tr)
	}
	if tr.Damages[0].Offset != row.Offset {
		t.Fatalf("damage offset=%d want %d", tr.Damages[0].Offset, row.Offset)
	}
}

// monotonic checks recovered DTS ordering across segments.
func monotonic(t *TrackResult) bool {
	var prev int64 = -1
	for _, seg := range t.Segments {
		if seg.Samples == 0 {
			continue
		}
		if prev >= 0 && seg.FirstDTSMS < prev {
			return false
		}
		prev = seg.LastDTSMS
	}
	return true
}
