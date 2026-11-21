package pipeline

import (
	"errors"
	"testing"

	"archivecheck.local/archivecheck/internal/fault"
	"archivecheck.local/archivecheck/internal/fixture"
	"archivecheck.local/archivecheck/internal/mkv"
)

// TestDamagePropagatesWithSourceOffset covers the brief's requirement that at
// least two damage classes reach the remux or verify stage with their source
// offset intact.
func TestDamagePropagatesWithSourceOffset(t *testing.T) {
	cases := []struct {
		kind    string
		stage   fault.Stage
		recover bool
	}{
		// Truncation is detected while re-reading the decoded tail and would
		// otherwise reach remux; strict mode stops at parse with the exact
		// first illegal byte.
		{kind: fixture.DamageTruncated, stage: fault.StageParse, recover: false},
		// A corrupt block is detected after de-lacing, in the demux stage.
		{kind: fixture.DamageBadBlock, stage: fault.StageDemux, recover: false},
		// An ambiguous size is never "recovered" and stays fatal.
		{kind: fixture.DamageOversized, stage: fault.StageParse, recover: true},
	}
	for _, c := range cases {
		_, err := runPipeline(t, c.kind, c.recover, 0)
		if err == nil {
			t.Fatalf("%s: expected an error", c.kind)
		}
		if got := fault.StageOf(err); got != c.stage {
			t.Fatalf("%s: stage=%q want %q (%v)", c.kind, got, c.stage, err)
		}
		if fault.OffsetOf(err) < 0 {
			t.Fatalf("%s: no source offset: %v", c.kind, err)
		}
		if !errors.Is(err, fault.ErrTruncated) && !errors.Is(err, fault.ErrOversized) &&
			!errors.Is(err, fault.ErrMalformed) && !errors.Is(err, fault.ErrBoundary) &&
			!errors.Is(err, fault.ErrCorruptBlock) && !errors.Is(err, mkv.ErrLacing) {
			t.Fatalf("%s: error not classified: %v", c.kind, err)
		}
		if fault.ExitCode(err) != 2 {
			t.Fatalf("%s: exit=%d", c.kind, fault.ExitCode(err))
		}
	}
}

func TestRecoveredOutputOnlyCoversProvenBytes(t *testing.T) {
	rep, err := runPipeline(t, fixture.DamageTruncated, true, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range rep.Manifest {
		for _, s := range rep.Skipped {
			if s.Length == 0 {
				continue
			}
			end := m.SourceOffset + int64(m.Size)
			if m.SourceOffset < s.Start+s.Length && end > s.Start {
				t.Fatalf("written sample overlaps a skipped range: sample=%+v skip=%+v", m, s)
			}
		}
	}
}
