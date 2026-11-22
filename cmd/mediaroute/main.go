// Command mediaroute rebuilds timelines, indexes and reports from
// generated HLS/DASH capture-node exports. Exit codes: 0 success within
// tolerance, 1 argument/budget/internal error, 2 corrupt input diagnosed.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"mediaroute/internal/clock"
	"mediaroute/internal/fixture"
	"mediaroute/internal/manifest"
	"mediaroute/internal/normalize"
	"mediaroute/internal/packet"
	"mediaroute/internal/report"
	"mediaroute/internal/stream"
)

const queueCapacity = 4

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 || args[0] != "demo" {
		fmt.Fprintln(stderr, "usage: mediaroute demo [--case default|truncated|slow-consumer] [--seed N] [--budget-mib N]")
		return 1
	}
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	fs.SetOutput(stderr)
	seed := fs.Int64("seed", 41, "fixture seed")
	budgetMiB := fs.Int64("budget-mib", 32, "pipeline memory budget in MiB")
	caseName := fs.String("case", "default", "fixture case")
	if err := fs.Parse(args[1:]); err != nil {
		return 1
	}
	if *budgetMiB <= 0 {
		fmt.Fprintln(stderr, "budget-mib must be positive")
		return 1
	}
	fx, err := fixture.Generate(*seed, *caseName)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	m, failures, err := pipeline(fx, *budgetMiB, *caseName == "slow-consumer", stderr)
	if err != nil {
		fmt.Fprintln(stderr, "pipeline:", err)
		return 1
	}
	m.Render(stdout)
	if len(failures) > 0 {
		for _, f := range failures {
			fmt.Fprintln(stderr, "diagnostic:", f)
		}
		return 2
	}
	return 0
}

// pipeline runs the full ingest: manifest parse, alignment, normalisation,
// bounded dispatch, packet repair, index rebuild and verification.
func pipeline(fx *fixture.Fixture, budgetMiB int64, slow bool, log io.Writer) (report.Metrics, []string, error) {
	m := report.Metrics{Fixture: "generated", TargetKbps: fixture.TargetKbps, BudgetMiB: budgetMiB}

	pl, err := manifest.ParseHLS(fx.HLS)
	if err != nil {
		return m, nil, err
	}
	mpd, err := manifest.ParseDASH(fx.MPD)
	if err != nil {
		return m, nil, err
	}
	ts, err := manifest.ProbeTimescale(bytes.NewReader(fx.Init))
	if err != nil {
		return m, nil, err
	}
	if ts != clock.VideoClockHz {
		return m, nil, fmt.Errorf("init segment timescale %d, want %d", ts, clock.VideoClockHz)
	}
	video := manifest.AlignVideo(pl)
	if _, err := manifest.AlignAudio(fx.Audio, video); err != nil {
		return m, nil, err
	}
	intervals, err := manifest.Merge(mpd, video)
	if err != nil {
		return m, nil, err
	}
	m.Segments = len(pl.Segments)

	dec, err := normalize.NormalizeAudio(fx.House, fx.Source)
	if err != nil {
		return m, nil, err
	}
	fmt.Fprintln(log, "audio:", dec.Note)
	for _, seg := range fx.Segments {
		d := normalize.ResolveColor(seg.Seq, seg.ContainerColor, seg.BitstreamColor)
		if d.Conflict {
			fmt.Fprintln(log, "color:", d.String())
		}
	}

	repairer := clock.NewRepairer(clock.VideoClockHz / 2) // 500 ms tolerance
	index := packet.NewBuilder()
	budget := stream.NewBudget(budgetMiB << 20)
	pipe := stream.NewPipe[int](queueCapacity)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	prodErr := make(chan error, 1)
	go func() {
		defer pipe.Close()
		for _, seg := range fx.Segments {
			if err := budget.Reserve(seg.PayloadBytes); err != nil {
				prodErr <- err
				cancel()
				return
			}
			if err := pipe.Send(ctx, seg.Seq); err != nil {
				budget.Release(seg.PayloadBytes)
				prodErr <- nil // cancelled or closed: keep completed segments
				return
			}
		}
		prodErr <- nil
	}()

	var totSE, totSamples float64
	for {
		seq, ok := pipe.Recv(ctx)
		if !ok {
			break
		}
		seg := fx.Segments[seq]
		processSegment(fx, seq, repairer, index, &m, &totSE, &totSamples, log)
		budget.Release(seg.PayloadBytes)
		if slow {
			time.Sleep(2 * time.Millisecond) // slow consumer: producer blocks on the full queue
		}
	}
	if err := <-prodErr; err != nil {
		return m, nil, err
	}

	m.PeakMemBytes = budget.Peak()
	m.IndexEntries = len(index.Entries())
	dir, err := os.MkdirTemp("", "mediaroute-index-*")
	if err != nil {
		return m, nil, err
	}
	defer os.RemoveAll(dir)
	if err := index.WriteAtomic(filepath.Join(dir, "index.jsonl")); err != nil {
		return m, nil, err
	}

	meanMs, maxMs, failures := packet.Verify(index.Entries(), intervals, clock.TicksPerFrame30)
	m.ManifestErrMs = meanMs
	m.AlignErrMs = maxMs
	m.PSNRdB = report.PSNR(totSE, uint64(totSamples))
	var bytesIndexed, ticksIndexed int64
	for _, e := range index.Entries() {
		bytesIndexed += e.Bytes
		ticksIndexed += fixture.SegmentDurationTicks
	}
	if ticksIndexed > 0 {
		m.ActualKbps = int(bytesIndexed * 8 * clock.VideoClockHz / (ticksIndexed * 1000))
	}
	m.DeltaPct = report.DeltaPercent(m.ActualKbps, m.TargetKbps)
	return m, failures, nil
}

// processSegment repairs one segment's timeline and folds it into the index.
func processSegment(fx *fixture.Fixture, seq int, repairer *clock.Repairer,
	index *packet.Builder, m *report.Metrics, totSE, totSamples *float64, log io.Writer) {
	seg := fx.Segments[seq]
	if seg.Missing {
		m.Unrecoverable++
		repairer.Skip(seg.DurationTicks)
		ce := &clock.Error{Op: "ingest", Segment: seq,
			StartTick: uint64(seq) * fixture.SegmentDurationTicks,
			EndTick:   uint64(seq+1) * fixture.SegmentDurationTicks,
			Msg:       "segment media missing at capture node"}
		fmt.Fprintln(log, "diagnostic:", ce)
		return
	}
	r := bytes.NewReader(fx.SegmentBytes(seq))
	var keyTS uint64
	haveKey := false
	lostKey := false
	for {
		pkt, flags, err := packet.ReadPacket(r)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if errors.Is(err, packet.ErrTruncated) || errors.Is(err, packet.ErrCorrupt) {
				m.RejectedPackets++
				if flags&packet.FlagKeyframe != 0 {
					lostKey = true
				}
				continue
			}
			break
		}
		if flags&packet.FlagKeyframe != 0 && !haveKey {
			keyTS, haveKey = uint64(pkt.Timestamp), true
		}
	}
	if lostKey {
		m.RecoveredSegments++ // remainder of the segment stayed decodable
	}
	if !haveKey {
		repairer.Skip(seg.DurationTicks) // no keyframe: nothing may enter the index
		return
	}
	adjusted, status, rep := repairer.Add(seq, keyTS, seg.DurationTicks)
	switch status {
	case clock.StatusRepaired:
		m.Repaired++
		fmt.Fprintf(log, "diagnostic: segment=%d repaired: %s (observed=%d adjusted=%d)\n",
			seq, rep.Reason, rep.Observed, rep.Adjusted)
	case clock.StatusUnrecoverable:
		m.Unrecoverable++
		fmt.Fprintf(log, "diagnostic: segment=%d unrecoverable: %s\n", seq, rep.Reason)
		return
	}
	if err := index.AddKeyframe(seq, adjusted, seg.PayloadBytes); err != nil {
		fmt.Fprintln(log, "diagnostic:", err)
		return
	}
	*totSE += seg.SumSqErr
	*totSamples += float64(seg.Samples)
}
