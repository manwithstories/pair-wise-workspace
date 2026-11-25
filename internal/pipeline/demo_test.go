package pipeline

import (
	"testing"

	"github.com/pairwise/mediad/internal/fixture"
	"github.com/pairwise/mediad/internal/media"
)

func TestCleanRunMetricsAndRepeatedHashes(t *testing.T) {
	out := t.TempDir()
	opts := Options{Scenario: fixture.ScenarioClean, Seed: 73, Out: out}
	res, err := Run(opts)
	if err != nil {
		t.Fatal(err)
	}
	if res.RoundTripRate != 100 || res.KeyframeRate != 100 {
		t.Fatalf("roundtrip=%f keyframe=%f", res.RoundTripRate, res.KeyframeRate)
	}
	if len(res.Repair.Segments) != 12 || res.HashMatched != 12 || res.HashTotal != 12 {
		t.Fatalf("segments=%d hash=%d/%d", len(res.Repair.Segments), res.HashMatched, res.HashTotal)
	}
	if res.Repair.Duplicates != 24 || res.Repair.BurstLosses != 4 || res.Repair.BandwidthSteps != 5 {
		t.Fatalf("dup=%d burst=%d steps=%d", res.Repair.Duplicates, res.Repair.BurstLosses, res.Repair.BandwidthSteps)
	}
	first := hashMap(res.SegmentHashes)

	res2, err := Run(opts)
	if err != nil {
		t.Fatal(err)
	}
	second := hashMap(res2.SegmentHashes)
	if len(first) != 12 || len(second) != 12 {
		t.Fatalf("hash rows %d/%d", len(first), len(second))
	}
	for path, h := range first {
		if second[path] != h {
			t.Fatalf("hash changed for %s: %s != %s", path, h, second[path])
		}
	}
}

func hashMap(rows []SegmentHash) map[string]string {
	m := make(map[string]string, len(rows))
	for _, r := range rows {
		m[r.RelPath] = r.SHA256
	}
	return m
}

func TestMissingInitBlocksExactNode(t *testing.T) {
	res, err := Run(Options{Scenario: fixture.ScenarioMissingInit, Seed: 73, Out: t.TempDir()})
	if media.ExitCode(err) != 3 {
		t.Fatalf("err=%v exit=%d", err, media.ExitCode(err))
	}
	if res.FirstBlocked != "repair:track-02:init-segment" {
		t.Fatalf("first_blocked=%q", res.FirstBlocked)
	}
}

func TestDamagedScenarioRejectsWithExitTwoAfterRecoveryMetrics(t *testing.T) {
	res, err := Run(Options{Scenario: fixture.ScenarioDamaged, Seed: 73, Out: t.TempDir()})
	if media.ExitCode(err) != 2 {
		t.Fatalf("err=%v", err)
	}
	if res == nil || res.HashMatched != 12 || res.Repair.Duplicates != 24 || res.Repair.BurstLosses != 4 {
		t.Fatalf("damaged result=%+v", res)
	}
	if res.DamageRejected == "" {
		t.Fatal("missing damage rejection evidence")
	}
}

func TestInvalidScenarioExitFour(t *testing.T) {
	_, err := Run(Options{Scenario: "nope", Seed: 1, Out: t.TempDir()})
	if media.ExitCode(err) != 4 {
		t.Fatalf("err=%v", err)
	}
}
