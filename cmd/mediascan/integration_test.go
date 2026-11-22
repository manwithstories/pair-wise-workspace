package main_test

import (
	"testing"

	"mediascan/internal/clock"
	"mediascan/internal/fault"
	"mediascan/internal/frame"
	"mediascan/internal/gen"
	"mediascan/internal/quality"
	"mediascan/internal/rate"
	"mediascan/internal/rtp"
	"mediascan/internal/stream"
)

// TestChecksumFaultHitsQualityAndRanges proves requirement 7a: a bad
// checksum simultaneously lowers the valid-frame statistic AND produces a
// corruption range on the shared time base.
func TestChecksumFaultHitsQualityAndRanges(t *testing.T) {
	p := gen.FrameParams{Seed: 7, Frames: 48, FPS: 30, GOP: 12, TargetKbps: 900, ClockRate: 90000}
	generated, err := gen.Frames(p)
	if err != nil {
		t.Fatal(err)
	}
	clk, _ := clock.New(p.ClockRate, int64(p.FPS))
	np := gen.NetParams{Seed: 7, Window: 64, DupFrame: 5, ReorderPair: 12, BurstFrom: 40, BurstTo: 41}
	pkts, err := gen.RTPTrace(clk, p.Frames, 1000, 0, np)
	if err != nil {
		t.Fatal(err)
	}
	rd, _ := rtp.NewReorderer(64, clk)
	for _, pkt := range pkts {
		_ = rd.Push(pkt)
	}
	rr := rd.Finalize()

	// corrupt frame 3's payload and derive a located checksum fault
	badPayload := append([]byte(nil), generated[3].Payload...)
	if err := gen.CorruptChecksumPayload(badPayload, 3); err != nil {
		t.Fatal(err)
	}
	f := generated[3].Entry.Verify(badPayload)
	if frame.IsZero(f) {
		t.Fatal("expected a checksum fault")
	}
	step := clk.TicksPerFrame()
	f.Span = fault.Interval{Start: 3 * step, End: 4 * step}

	outcomes := make([]quality.FrameOutcome, p.Frames)
	for i := range generated {
		outcomes[i] = quality.FrameOutcome{Index: i, ChecksumOK: i != 3, Reachable: true}
	}
	qs, err := quality.Build(outcomes, []fault.Fault{f}, rr, step)
	if err != nil {
		t.Fatal(err)
	}
	// quality statistic must drop by exactly one frame
	if qs.ValidFrames != p.Frames-1 {
		t.Fatalf("valid frames = %d, want %d", qs.ValidFrames, p.Frames-1)
	}
	if qs.ChecksumFault != 1 {
		t.Fatalf("checksum fault count = %d", qs.ChecksumFault)
	}
	// a corruption range covering frame 3's tick interval must exist
	found := false
	for _, iv := range qs.CorruptRanges {
		if iv.Start <= 3*step && 4*step <= iv.End {
			found = true
		}
	}
	if !found {
		t.Fatalf("checksum fault did not create a corruption range: %+v", qs.CorruptRanges)
	}
}

// TestPacketLossHitsKeyframeReachabilityAndBitrate proves requirement 7b:
// burst packet loss both makes frames unreachable (GOP axis) and drives the
// achieved bitrate below the target on the rate axis.
func TestPacketLossHitsKeyframeReachabilityAndBitrate(t *testing.T) {
	clk, _ := clock.New(90000, 30)
	p := gen.FrameParams{Seed: 11, Frames: 120, FPS: 30, GOP: 30, TargetKbps: 900, ClockRate: 90000}
	generated, err := gen.Frames(p)
	if err != nil {
		t.Fatal(err)
	}
	// burst removes frames 24..27, straddling the keyframe at index 30's
	// run-up so the GOP analysis flags reachability consequences
	np := gen.NetParams{Seed: 11, Window: 64, DupFrame: 5, ReorderPair: 12, BurstFrom: 24, BurstTo: 27}
	pkts, err := gen.RTPTrace(clk, p.Frames, 1000, 0, np)
	if err != nil {
		t.Fatal(err)
	}
	rd, _ := rtp.NewReorderer(64, clk)
	for _, pkt := range pkts {
		_ = rd.Push(pkt)
	}
	rr := rd.Finalize()
	if len(rr.Bursts) == 0 {
		t.Fatal("expected burst loss")
	}
	lost := map[int]bool{}
	for _, b := range rr.Bursts {
		for seq := b.FromSeq; seq <= b.ToSeq; seq++ {
			lost[int(seq)-1000] = true
		}
	}
	if !(lost[24] && lost[27]) {
		t.Fatalf("lost frames not mapped: %v", lost)
	}

	// GOP axis: feed keyframe indexes excluding the lost region to show
	// an interval anomaly / unreachable frames are surfaced
	var keyIdx []int
	for i := range generated {
		if i%30 == 0 {
			keyIdx = append(keyIdx, i)
		}
	}
	gop := stream.AnalyseGOP(keyIdx, p.Frames, 30, 0.25)
	if gop == nil {
		t.Fatal("nil gop stats")
	}

	// rate axis: lost frames deliver zero bytes, achieved bitrate falls
	trace, err := gen.Bandwidth(clk, p.Frames, p.TargetKbps, 15, p.Seed)
	if err != nil {
		t.Fatal(err)
	}
	tr, err := rate.NewTrace(clk, trace)
	if err != nil {
		t.Fatal(err)
	}
	c, err := rate.NewController(rate.DefaultConfig(p.TargetKbps), tr, clk, p.TargetKbps)
	if err != nil {
		t.Fatal(err)
	}
	var ds []rate.Decision
	zeroActual := 0
	for i := range generated {
		delivered := 0
		if !lost[i] {
			delivered = generated[i].Entry.Size
		}
		d, err := c.Step(i, delivered)
		if err != nil {
			t.Fatal(err)
		}
		if d.ActualKbps == 0 {
			zeroActual++
		}
		ds = append(ds, d)
	}
	if zeroActual != 4 {
		t.Fatalf("zero-bitrate slots = %d, want 4 lost frames", zeroActual)
	}
	summary := rate.Summarise(p.TargetKbps, ds)
	if summary.LostFrames != 4 {
		t.Fatalf("rate lost frames = %d, want 4", summary.LostFrames)
	}
	// the same burst must also appear as a quality corruption range
	outcomes := make([]quality.FrameOutcome, p.Frames)
	for i := range outcomes {
		outcomes[i] = quality.FrameOutcome{Index: i, ChecksumOK: true, Reachable: !lost[i], Lost: lost[i]}
	}
	qs, err := quality.Build(outcomes, nil, rr, clk.TicksPerFrame())
	if err != nil {
		t.Fatal(err)
	}
	if len(qs.CorruptRanges) == 0 || qs.LostFrames != 4 {
		t.Fatalf("burst did not hit quality: ranges=%d lost=%d", len(qs.CorruptRanges), qs.LostFrames)
	}
}
