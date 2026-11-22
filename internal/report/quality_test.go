package report

import (
	"testing"
	"time"

	"mediacq/internal/clock"
)

func TestBitrateAndDelta(t *testing.T) {
	b := NewBuilder(4, 2_500_000)
	// 2,500,000 bits = 312,500 bytes over 1 second -> exactly target.
	b.SetBitrate(312500, time.Second)
	m := b.Snapshot()
	if m.ActualBPS != 2_500_000 {
		t.Fatalf("actual bps = %d", m.ActualBPS)
	}
	if m.BitrateDeltaPct != 0 {
		t.Fatalf("delta = %.4f", m.BitrateDeltaPct)
	}
	if !m.AssertBitrate(2.0) {
		t.Fatal("exact-target bitrate must pass a 2% assertion")
	}
}

func TestBitrateToleranceBoundary(t *testing.T) {
	b := NewBuilder(4, 2_500_000)
	// 306,250 bytes/s -> 2,450,000 bps = -2%.
	b.SetBitrate(306250, time.Second)
	m := b.Snapshot()
	if !m.AssertBitrate(2.0) {
		t.Fatalf("exactly -2%% should pass, delta=%.3f", m.BitrateDeltaPct)
	}
	b2 := NewBuilder(4, 2_500_000)
	b2.SetBitrate(300000, time.Second) // 2.4 Mbps = -4%
	if b2.Snapshot().AssertBitrate(2.0) {
		t.Fatal("-4% must fail a 2% assertion")
	}
}

func TestSilenceAccumulation(t *testing.T) {
	b := NewBuilder(4, 0)
	r1 := clock.Range{Start: 0, End: clock.Time(120 * time.Millisecond)}
	r2 := clock.Range{Start: clock.Time(time.Second), End: clock.Time(1100 * time.Millisecond)}
	if err := b.AddSilence(r1); err != nil {
		t.Fatal(err)
	}
	if err := b.AddSilence(r2); err != nil {
		t.Fatal(err)
	}
	m := b.Snapshot()
	if m.SilenceSpans != 2 || m.SilenceMS != 220 {
		t.Fatalf("silence spans=%d ms=%d", m.SilenceSpans, m.SilenceMS)
	}
}

func TestErrorsContentFreeAndCounted(t *testing.T) {
	b := NewBuilder(4, 0)
	b.AddError(QualityError{Code: "missing_keyframe", TrackID: 1, Offset: 2048, RawTS: 0})
	b.AddError(QualityError{Code: "corrupt_box", TrackID: 0, Offset: 355})
	m := b.Snapshot()
	if b.QualityErrors() != 2 || len(m.Errors) != 2 {
		t.Fatal("error count mismatch")
	}
	for _, e := range m.Errors {
		if e.Code == "" {
			t.Fatal("error code required")
		}
	}
}

func TestAlignmentAssertion(t *testing.T) {
	b := NewBuilder(4, 0)
	b.SetAlignment(20)
	if !b.Snapshot().AssertAlignment(20) {
		t.Fatal("20 ms must pass a 20 ms limit")
	}
	b.SetAlignment(21)
	if b.Snapshot().AssertAlignment(20) {
		t.Fatal("21 ms must fail a 20 ms limit")
	}
}

func TestSnapshotIsCopy(t *testing.T) {
	b := NewBuilder(4, 0)
	b.AddError(QualityError{Code: "x"})
	m := b.Snapshot()
	m.Errors[0].Code = "mutated"
	if b.Snapshot().Errors[0].Code != "x" {
		t.Fatal("snapshot must be an independent copy")
	}
}
