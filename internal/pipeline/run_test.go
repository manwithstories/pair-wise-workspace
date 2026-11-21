package pipeline

import (
	"bytes"
	"context"
	"io"
	"testing"

	"avcheck/internal/fixture"
	"avcheck/internal/metrics"
)

func smallConfig() fixture.Config {
	c := fixture.DefaultConfig()
	c.DurationMS = 1000
	c.Tracks = 3
	c.LossBurst = 3
	c.Reorder = 6
	c.Duplicate = 4
	return c
}

func TestNormalRunPassesThresholds(t *testing.T) {
	cfg := smallConfig()
	g := fixture.NewGenerator(cfg)
	g.BuildTracks()
	pkts := g.Normal()

	var out bytes.Buffer
	res, err := Run(context.Background(), Params{
		Fixture: cfg, QueueBytes: 262144, WindowPkts: 16, Out: &out,
	}, pkts)
	if err != nil {
		t.Fatal(err)
	}
	if out.Len() == 0 {
		t.Fatal("MP4 output is empty")
	}
	th := metrics.DefaultThresholds(262144)
	if !res.Report.Pass(th) {
		t.Fatalf("healthy run failed gates: %s", res.Report.Line())
	}
	if res.Report.SyncErrorSamples > 1 {
		t.Fatalf("sync errors=%d", res.Report.SyncErrorSamples)
	}
}

func TestLossRecoveryPropagatesIntoReport(t *testing.T) {
	cfg := smallConfig()
	g := fixture.NewGenerator(cfg)
	g.BuildTracks()
	pkts, dropped := g.LossBurst()

	var out bytes.Buffer
	res, err := Run(context.Background(), Params{
		Fixture: cfg, QueueBytes: 262144, WindowPkts: 32, Out: &out,
	}, pkts)
	if err != nil {
		t.Fatal(err)
	}
	var droppedCount int
	for _, seqs := range dropped {
		droppedCount += len(seqs)
	}
	if droppedCount == 0 {
		t.Fatal("fixture produced no drops")
	}
	// Every 3-packet burst must be interpolated -> full recovery rate.
	if res.Recovered.Interpolated == 0 {
		t.Fatalf("no interpolation happened, stats=%+v", res.Recovered)
	}
	if res.Report.LossRecoveryRate < 0.9 {
		t.Fatalf("recovery rate=%v below 0.9", res.Report.LossRecoveryRate)
	}
	if res.Report.FrameHash == 0 {
		t.Fatal("frame hash not computed")
	}
}

func TestQueueFullDrainsWithoutLosingAcceptedBlocks(t *testing.T) {
	cfg := smallConfig()
	g := fixture.NewGenerator(cfg)
	g.BuildTracks()
	pkts := g.Normal()

	// Tiny ceiling forces repeated backpressure while the consumer drains.
	res, err := Run(context.Background(), Params{
		Fixture: cfg, QueueBytes: 4096, WindowPkts: 16, Out: io.Discard,
	}, pkts)
	if err != nil {
		t.Fatal(err)
	}
	if res.Queue.BackpressureEv == 0 {
		t.Fatal("expected backpressure events under a tiny queue")
	}
	if res.Queue.PeakBytes > 4096 {
		t.Fatalf("peak=%d exceeded 4096 ceiling", res.Queue.PeakBytes)
	}
	// All accepted blocks must survive: late/dropped are the only losses and
	// the normal fixture has neither.
	if res.LatePackets != 0 {
		t.Fatalf("unexpected late packets=%d", res.LatePackets)
	}
	wantFrames := cfg.SampleCount()
	var total int
	for _, n := range res.Report.TrackSamples {
		total += n
	}
	if total != wantFrames*cfg.Tracks {
		t.Fatalf("emitted frames=%d, want %d", total, wantFrames*cfg.Tracks)
	}
}

func TestDeterministicSummaryAcrossRuns(t *testing.T) {
	cfg := smallConfig()
	build := func() (string, []byte) {
		g := fixture.NewGenerator(cfg)
		g.BuildTracks()
		var out bytes.Buffer
		res, err := Run(context.Background(), Params{
			Fixture: cfg, QueueBytes: 262144, WindowPkts: 16, Out: &out,
		}, g.Normal())
		if err != nil {
			t.Fatal(err)
		}
		return res.Report.Line(), out.Bytes()
	}
	line1, mp41 := build()
	line2, mp42 := build()
	if line1 != line2 {
		t.Fatalf("nondeterministic report:\n%s\n%s", line1, line2)
	}
	if !bytes.Equal(mp41, mp42) {
		t.Fatal("nondeterministic MP4 bytes")
	}
}
