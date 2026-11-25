package manifest_test

import (
	"testing"

	"mediacheck/internal/manifest"
	"mediacheck/internal/media"
)

func TestAlignDurationsWithinOneMillisecond(t *testing.T) {
	// Eight 90 kHz ticks are ~89 us.
	a, err := manifest.AlignDurations(180000, 180008, 180000)
	if err != nil {
		t.Fatal(err)
	}
	if a.SkewMicroseconds != 88 {
		t.Fatalf("skew us=%d ticks=%d", a.SkewMicroseconds, a.SkewTicks)
	}
	if err := a.CheckCounts(60, 96000, 2, 2); err != nil {
		t.Fatal(err)
	}
}

func TestAlignDurationsRejectsExcessiveSkew(t *testing.T) {
	_, err := manifest.AlignDurations(180000, 180200, 180000)
	if err == nil {
		t.Fatal("expected skew failure")
	}
	me, ok := media.AsError(err)
	if !ok || me.Stage != media.StageManifest {
		t.Fatalf("err=%v", err)
	}
}

func TestCheckCountsRejectsIndexMismatch(t *testing.T) {
	a, err := manifest.AlignDurations(100, 100, 100)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.CheckCounts(30, 1000, 2, 1); err == nil {
		t.Fatal("expected keyframe count mismatch")
	}
}
