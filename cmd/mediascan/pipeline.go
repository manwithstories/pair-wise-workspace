package main

import (
	"bytes"
	"fmt"

	"mediascan/internal/batch"
	"mediascan/internal/clock"
	"mediascan/internal/fault"
	"mediascan/internal/frame"
	"mediascan/internal/gen"
	"mediascan/internal/quality"
	"mediascan/internal/rate"
	"mediascan/internal/rtp"
	"mediascan/internal/stream"
)

const rtpWindowCap = 8192

// Report carries every headline metric printed by the CLI.
type Report struct {
	Frames           int
	GOPp50           int
	Dup              int
	Reordered        int
	BurstLoss        int
	TargetKbps       int
	ActualKbps       int
	ErrorPctX10      int
	ValidPctX100     int
	AlignmentMS      int64
	CorruptRanges    int
	CheckpointChunks int
	FatalFaults      int
	RecoveredChunks  int
	InputDigest      string
	FirstTick        int64
	LastTick         int64
}

// Run executes the full analysis pipeline and returns the report and
// terminal exit code.
func Run(o Options) (Report, int) {
	var rep Report
	clk, err := clock.New(90000, int64(o.FPS))
	if err != nil {
		return rep, exitCorruptInput
	}
	// 1. deterministic frame generation
	fp := gen.FrameParams{
		Seed: o.Seed, Frames: o.Frames, FPS: o.FPS, GOP: o.GOP,
		TargetKbps: o.TargetKbps, ClockRate: clk.Rate,
	}
	generated, err := gen.Frames(fp)
	if err != nil {
		fmt.Fprintln(stderr(), "generate:", err)
		return rep, exitCorruptInput
	}
	rep.Frames = len(generated)
	manifest := buildManifest(o, generated)
	mres, err := manifest.Validate()
	if err != nil {
		fmt.Fprintln(stderr(), "manifest:", err)
		return rep, exitCorruptInput
	}
	rep.AlignmentMS = mres.AlignmentMS

	// 2. MP4 container structural scan (with optional truncation)
	containerFaults := scanContainer(o, generated)

	// 3. checksum verification, applying the optional corruption
	checksumFaults := verifyFrames(o, generated)

	// 4. RTP trace generation and bounded reorder
	rr := runRTP(clk, o)
	rep.Dup = rr.Duplicates
	rep.Reordered = rr.Reordered
	rep.BurstLoss = len(rr.Bursts)

	// 5. GOP statistics on the shared frame axis
	gop := stream.AnalyseGOP(mres.KeyframeIdx, o.Frames, o.GOP, 0.25)
	rep.GOPp50 = gop.P50

	// 6. bitrate controller over the bandwidth trace
	rateSummary := runRate(clk, o, generated, rr)
	rep.TargetKbps = rateSummary.TargetKbps
	rep.ActualKbps = rateSummary.MeanActual
	rep.ErrorPctX10 = rateSummary.MeanErrorPctX10

	// 7. quality outcomes combining checksum, loss and reachability
	outcomes := buildOutcomes(o, generated, rr, gop)
	qs, err := quality.Build(outcomes, checksumFaults, rr, int64(clk.TicksPerFrame()))
	if err != nil {
		fmt.Fprintln(stderr(), "quality:", err)
		return rep, exitIndexFault
	}
	rep.ValidPctX100 = qs.ValidPctX100
	rep.CorruptRanges = len(qs.CorruptRanges)

	// 8. collect every fatal fault to decide the input-invalid exit code
	allFaults := appendFaults(mres.Faults, containerFaults, checksumFaults, qs.Faults, gop.Faults, rr.Faults)
	rep.FatalFaults = fault.FatalCount(allFaults)

	// 9. chunked index + checkpoint with resume semantics
	code := commitChunks(o, clk, generated, qs, outcomes, &rep)
	return rep, code
}

func buildManifest(o Options, g []gen.Generated) *frame.Manifest {
	m := &frame.Manifest{
		FPS: o.FPS, ClockRate: 90000, GOP: o.GOP,
		TargetKbps: o.TargetKbps, Frames: make([]frame.Frame, len(g)),
	}
	for i := range g {
		m.Frames[i] = g[i].Entry
	}
	return m
}

func scanContainer(o Options, g []gen.Generated) []fault.Fault {
	// concatenate a bounded payload slice to form the mdat body
	var body bytes.Buffer
	for i := range g {
		body.Write(g[i].Payload)
	}
	var data []byte
	if gen.HasCorruption(o.Corrupt, gen.CorruptTruncateBox) {
		corrupt, err := gen.TruncatedMP4(body.Bytes(), 64)
		if err != nil {
			return []fault.Fault{{Cat: fault.CatTruncatedBox, Msg: err.Error()}}
		}
		data = corrupt
	} else {
		full, err := stream.BuildMinimalMP4(body.Bytes())
		if err != nil {
			return []fault.Fault{{Cat: fault.CatIllegalSize, Msg: err.Error()}}
		}
		data = full
	}
	a, err := stream.Scan(bytes.NewReader(data))
	if err != nil {
		return []fault.Fault{{Cat: fault.CatIllegalSize, Msg: err.Error()}}
	}
	return a.Faults
}

func verifyFrames(o Options, g []gen.Generated) []fault.Fault {
	var fs []fault.Fault
	corruptChecksum := gen.HasCorruption(o.Corrupt, gen.CorruptChecksum)
	for i := range g {
		payload := g[i].Payload
		if corruptChecksum && i == corruptFrameIndex {
			payload = append([]byte(nil), payload...)
			_ = gen.CorruptChecksumPayload(payload, i)
		}
		if f := g[i].Entry.Verify(payload); !frame.IsZero(f) {
			// locate on the media tick grid
			step := int64(90000) / int64(o.FPS)
			f.Span = fault.Interval{Start: int64(i) * step, End: int64(i+1) * step}
			fs = append(fs, f)
		}
	}
	return fs
}

const corruptFrameIndex = 3

func runRTP(clk clock.Clock, o Options) *rtp.Result {
	np := gen.DefaultNetParams(o.Seed)
	np.Window = o.ReorderWindow
	pkts, err := gen.RTPTrace(clk, o.Frames, 1000, 0, np)
	if err != nil {
		// impairment positions are guaranteed valid for frames >= 40
		panic(fmt.Sprintf("rtp trace: %v", err))
	}
	reorderer, _ := rtp.NewReorderer(o.ReorderWindow, clk)
	for _, p := range pkts {
		_ = reorderer.Push(p)
	}
	return reorderer.Finalize()
}

func runRate(clk clock.Clock, o Options, g []gen.Generated, rr *rtp.Result) rate.Summary {
	trace, _ := gen.Bandwidth(clk, o.Frames, o.TargetKbps, 15, o.Seed)
	tr, err := rate.NewTrace(clk, trace)
	if err != nil {
		panic(err)
	}
	c, err := rate.NewController(rate.DefaultConfig(o.TargetKbps), tr, clk, o.TargetKbps)
	if err != nil {
		panic(err)
	}
	lost := map[int]bool{}
	for _, b := range rr.Bursts {
		// map extended seq back to frame index using the trace base
		for seq := b.FromSeq; seq <= b.ToSeq; seq++ {
			if fi := int(seq) - 1000; fi >= 0 && fi < len(g) {
				lost[fi] = true
			}
		}
	}
	var ds []rate.Decision
	for i := range g {
		bytes := 0
		if !lost[i] {
			bytes = g[i].Entry.Size
		}
		d, err := c.Step(i, bytes)
		if err != nil {
			panic(err)
		}
		ds = append(ds, d)
	}
	return rate.Summarise(o.TargetKbps, ds)
}

func buildOutcomes(o Options, g []gen.Generated, rr *rtp.Result, gop *stream.GOPStats) []quality.FrameOutcome {
	lost := map[int]bool{}
	for _, b := range rr.Bursts {
		for seq := b.FromSeq; seq <= b.ToSeq; seq++ {
			if fi := int(seq) - 1000; fi >= 0 && fi < len(g) {
				lost[fi] = true
			}
		}
	}
	unreachable := map[int]bool{}
	for _, fi := range gop.Unreachable {
		unreachable[fi] = true
	}
	corruptChecksum := gen.HasCorruption(o.Corrupt, gen.CorruptChecksum)
	out := make([]quality.FrameOutcome, len(g))
	for i := range g {
		out[i] = quality.FrameOutcome{
			Index:      i,
			ChecksumOK: !(corruptChecksum && i == corruptFrameIndex),
			Reachable:  !unreachable[i],
			Lost:       lost[i],
		}
	}
	return out
}

func appendFaults(groups ...[]fault.Fault) []fault.Fault {
	var out []fault.Fault
	for _, g := range groups {
		out = append(out, g...)
	}
	return out
}

// digestInput derives the stable input/params digest for resume checks.
func digestInput(o Options) (string, string) {
	in := batch.Digest(
		[]byte(fmt.Sprintf("%d|%d|%d|%d", o.Seed, o.Frames, o.FPS, o.GOP)),
	)
	params := batch.Digest(
		[]byte(fmt.Sprintf("%d|%d|%s", o.TargetKbps, o.ChunkFrames, o.BandwidthTrace)),
	)
	return in, params
}
