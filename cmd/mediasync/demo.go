package main

import (
	"fmt"
	"math/rand"
	"path/filepath"
	"sort"

	"mediasync/internal/fixture"
	"mediasync/internal/metadata"
	"mediasync/internal/model"
	"mediasync/internal/render"
	"mediasync/internal/report"
	"mediasync/internal/timeline"
	"mediasync/internal/transport"
)

// runDemo executes the full pipeline and prints the acceptance block.
func runDemo(args []string) error {
	opts, err := parseFlags(args)
	if err != nil {
		return err
	}
	rng := rand.New(rand.NewSource(opts.seed))
	master := model.Clock{TickRate: 1000} // shared 1kHz simulation clock
	videoRate := model.Rate2997
	reps := []transport.Representation{
		{Name: "low", Bitrate: 200_000},
		{Name: "mid", Bitrate: 700_000},
		{Name: "high", Bitrate: 2_500_000},
	}
	schedule := bandwidthScheduler(opts.bandwidths, opts.durationMs)

	// 1. Metadata fixtures with rotation/crop/color conflicts.
	metaSet, err := fixture.BuildMetadata(fixture.Config{
		Seed: opts.seed, DurationTicks: opts.durationMs, LossBurstTicks: opts.burstMs,
		VideoRate: videoRate, Master: master, Reps: reps,
	})
	if err != nil {
		return fmt.Errorf("metadata fixtures: %w", err)
	}
	resolved, metaDiag := resolveMetadata(metaSet)
	fmt.Printf("metadata: %d tracks, %d conflict/field diagnostics, %d policy decisions\n",
		len(resolved), metaDiag.Len(), decisionCount(resolved))

	// 2. Synthetic RTP stream with duplicates, reordering and a loss burst.
	logicalEvents, lossInjected := fixture.GenerateVideoStream(rng, fixture.Config{
		Seed: opts.seed, DurationTicks: opts.durationMs, LossBurstTicks: opts.burstMs,
		VideoRate: videoRate, Master: master, Reps: reps, BandwidthSchedule: schedule,
	})
	events := fixture.Perturb(rng, logicalEvents, 25, 6)

	// Logical ABR boundaries come from the pre-reorder send stream, so network
	// reordering cannot manufacture spurious switches.
	type boundary struct {
		rep  int
		tick int64
	}
	var boundaries []boundary
	prevRep := -1
	for _, ev := range logicalEvents {
		if ev.Dropped {
			continue
		}
		if ev.Packet.RepIndex != prevRep {
			boundaries = append(boundaries, boundary{ev.Packet.RepIndex, ev.Packet.SendTicks})
			prevRep = ev.Packet.RepIndex
		}
	}
	// Switches are reported on the monotonic shared clock regardless of how the
	// piecewise schedule's segments interleave in the generated stream.
	sort.Slice(boundaries, func(i, j int) bool { return boundaries[i].tick < boundaries[j].tick })

	fan, err := transport.NewFanout(transport.FanoutConfig{
		Track: "cam1", Clock: model.Clock90k, Reps: reps, EstimatorAlpha: 0.4,
		TickRate: master.TickRate, HoldTicks: 300,
		Reorder: transport.ReorderConfig{Window: 64}, ExternalSelection: true,
	})
	if err != nil {
		return err
	}
	for _, b := range boundaries {
		if err := fan.NoteSwitch(b.rep, b.tick); err != nil {
			return err
		}
	}
	var allFrames []model.Frame
	var latencies []report.LatencySample
	for _, ev := range events {
		if ev.Dropped {
			continue
		}
		frames, err := fan.Push(ev.Packet)
		if err != nil {
			return err
		}
		for _, f := range frames {
			allFrames = append(allFrames, f)
			latencies = append(latencies, report.LatencySample{
				Track: f.Track, Ticks: f.RecvTicks - f.SendTicks, Clock: master})
		}
	}
	allFrames = append(allFrames, fan.Flush()...)

	// 3. Audio frames with bounded jitter keep AV sync inside the acceptance band.
	audioFrames := buildAudioTrack(opts.durationMs, rng)

	// 4. Shared-clock timeline and boundary cross-check.
	inputs := []timeline.Input{
		{Track: resolved["cam1"], Frames: allFrames},
		{Track: resolved["mic1"], Frames: audioFrames},
	}
	tl := timeline.Merge(master, inputs, model.Rational{Num: 40, Den: 1000})
	for _, d := range metaDiag.All() {
		tl.Diag.Add(d)
	}
	frags := timeline.Fragmentize(tl)
	mismatches, boundaryDiag := timeline.VerifyBoundaries(tl, frags)
	timeline.MergeDiag(tl, boundaryDiag)

	// 5. Offscreen export with path-traversal protection.
	// Validate the raw user path (no pre-Clean) so traversal attempts are rejected
	// before any directory is created.
	if err := render.ValidateOutputPath(opts.output); err != nil {
		return err
	}
	exporter, err := render.NewExporter(opts.output)
	if err != nil {
		return err
	}
	manifest, err := exporter.Export(tl, mismatches)
	if err != nil {
		return err
	}

	// 6. Metrics: injected loss is the real denominator.
	metrics := report.Build(tl, fan.RepStats(), latencies, len(mismatches),
		manifest.HashCount, lossInjected)

	printAcceptance(metrics, fan, manifest, exporter.Root())
	return nil
}

// resolveMetadata groups MXF/MP4 records per logical track and applies policy.
func resolveMetadata(set fixture.MetadataSet) (map[string]metadata.ResolvedTrack, model.Bag) {
	var diag model.Bag
	groups := metadata.GroupByTrackID(set.MXFResult, set.MP4Result)
	out := map[string]metadata.ResolvedTrack{}
	for id, records := range groups {
		rt, d := metadata.Resolve(metadata.DefaultPolicy(), records)
		out[id] = rt
		for _, x := range d.All() {
			diag.Add(x)
		}
	}
	return out, diag
}

// bandwidthScheduler turns the three CLI values into a piecewise schedule
// across thirds of the clip (including the extreme middle cliff).
func bandwidthScheduler(values []int64, durationMs int64) func(int64) int64 {
	third := durationMs / 3
	return func(tick int64) int64 {
		idx := int(tick / third)
		if idx >= len(values) {
			idx = len(values) - 1
		}
		return values[idx]
	}
}

// buildAudioTrack creates 48kHz audio blocks with deterministic bounded jitter.
func buildAudioTrack(durationMs int64, rng *rand.Rand) []model.Frame {
	clock := model.Clock48k
	block := int64(1024)
	n := durationMs * clock.TickRate / 1000 / block
	out := make([]model.Frame, 0, n)
	for i := int64(0); i < n; i++ {
		// +/-8 ticks (~0.17ms) jitter: realistic but well within 40ms sync budget.
		jitter := int64(rng.Intn(17)) - 8
		out = append(out, model.Frame{
			Track: "mic1", Kind: model.KindAudio, Rep: "voice", Seq: i,
			Timestamp: i*block + jitter, Clock: clock, Payload: []byte{byte(i)},
			SendTicks: i * 1000 * block / clock.TickRate,
		})
	}
	return out
}

func decisionCount(resolved map[string]metadata.ResolvedTrack) int {
	n := 0
	for _, r := range resolved {
		n += len(r.Decisions)
	}
	return n
}

func printAcceptance(m report.Metrics, fan *transport.Fanout, manifest *render.Manifest, root string) {
	fmt.Println("=== mediasync acceptance report ===")
	for _, line := range m.Lines() {
		fmt.Println(line)
	}
	fmt.Printf("RECOVERY_GAP_COUNT=%d\n", gapTotal(fan))
	fmt.Printf("SWITCH_COUNT=%d\n", len(fan.Switches()))
	fmt.Println("per-representation recovery:")
	for _, st := range fan.RepStats() {
		fmt.Printf("  rep=%s recovered=%d gaps=%d duplicates=%d emitted=%d\n",
			st.Rep, st.Recovered, st.Gaps, st.Duplicates, st.Emitted)
	}
	fmt.Printf("incomplete_tracks=%v\n", m.IncompleteTracks)
	fmt.Printf("manifest=%s checksum=%s frames=%d\n",
		filepath.Join(root, "manifest.json"), manifest.Checksum, manifest.FrameCount)
	fmt.Println("boundary mismatch summary:")
	if len(manifest.Mismatches) == 0 {
		fmt.Println("  none")
	}
	for _, mm := range manifest.Mismatches {
		fmt.Printf("  track=%s tick=%d %s: %s\n", mm.TrackID, mm.AtTick, mm.Kind, mm.Message)
	}
	switchSummary(fan)
}

func gapTotal(fan *transport.Fanout) int {
	n := 0
	for _, st := range fan.RepStats() {
		n += st.Gaps
	}
	return n
}

func switchSummary(fan *transport.Fanout) {
	if len(fan.Switches()) == 0 {
		return
	}
	fmt.Println("representation switches (shared-clock boundaries):")
	for _, sw := range fan.Switches() {
		fmt.Printf("  tick=%d %s -> %s\n", sw.AtTicks, sw.FromRep, sw.ToRep)
	}
}
