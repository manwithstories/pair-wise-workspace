package pipeline

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/pairwise/mediapack/internal/audio"
	"github.com/pairwise/mediapack/internal/fixture"
)

func TestCCIntervalStaysAlignedUnderSlowConsumer(t *testing.T) {
	s, err := fixture.Generate(fixture.Config{Seed: 73, Faults: []string{"cc-gap", "psi-update"}})
	if err != nil {
		t.Fatal(err)
	}
	rep, err := Run(context.Background(), bytes.NewReader(s.TS), Options{
		BufferPackets: 2,
		ConsumerDelay: time.Millisecond,
		Silence:       audio.Config{SampleRate: fixture.SampleRate, MinSilenceMS: fixture.SilenceMS},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.ClockGaps) != 1 {
		t.Fatalf("clock gaps=%+v", rep.ClockGaps)
	}
	gap := rep.ClockGaps[0]
	if gap.Start != 120*time.Millisecond || gap.End != 160*time.Millisecond {
		t.Fatalf("gap interval=%s..%s", gap.Start, gap.End)
	}
	if len(rep.Cut.Segments[0].Markers) != 1 {
		t.Fatalf("first segment markers=%+v", rep.Cut.Segments[0].Markers)
	}
}
