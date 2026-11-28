// Command archive is the terminal entry point for the media archive
// recovery pipeline. Exit codes: 0 = artifacts verified, 2 = invalid
// metadata, 3 = unrecoverable anchors or integrity threshold missed,
// 4 = cancelled.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"archive/internal/fixture"
	"archive/internal/ingest"
	"archive/internal/media"
	"archive/internal/normalize"
	"archive/internal/recovery"
	"archive/internal/render"
	"archive/internal/report"
	"archive/internal/timebase"
	"archive/internal/timeline"
	"archive/internal/worker"
)

const (
	exitOK            = 0
	exitInvalidMeta   = 2
	exitIntegrity     = 3
	exitCancelled     = 4
	maxAVSyncUs       = 20.9 // one 48 kHz sample period, plus rounding slack
	minRecoveryRate   = 0.85
)

type options struct {
	kase    string
	seed    int64
	workers int
	runs    int
}

type runResult struct {
	metrics   report.Metrics
	manifest  string
	decisions []string
	faults    []*media.Fault
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout))
}

func run(args []string, w io.Writer) int {
	if len(args) < 1 || args[0] != "demo" {
		fmt.Fprintln(w, "usage: archive demo --case clean|damaged|invalid [--seed N] [--workers N] [--runs N]")
		return exitInvalidMeta
	}
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	fs.SetOutput(w)
	opts := options{}
	fs.StringVar(&opts.kase, "case", "clean", "fixture case: clean|damaged|invalid")
	fs.Int64Var(&opts.seed, "seed", 1, "deterministic fixture seed")
	fs.IntVar(&opts.workers, "workers", 2, "worker pool size (bounded)")
	fs.IntVar(&opts.runs, "runs", 1, "pipeline repetitions for determinism check")
	if err := fs.Parse(args[1:]); err != nil {
		return exitInvalidMeta
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var last *runResult
	hashes := map[string]int{}
	for r := 0; r < opts.runs; r++ {
		res, err := runPipeline(ctx, opts)
		if err != nil {
			return exitFor(err, w)
		}
		hashes[res.metrics.ManifestSHA]++
		last = res
	}
	fmt.Fprintf(w, "case=%s seed=%d workers=%d runs=%d\n", opts.kase, opts.seed, opts.workers, opts.runs)
	for _, f := range last.faults {
		fmt.Fprintf(w, "fault=%s\n", f)
	}
	for _, d := range last.decisions {
		fmt.Fprintf(w, "rule=%s\n", d)
	}
	if len(hashes) != 1 {
		fmt.Fprintf(w, "hash_stable=false distinct_manifests=%d\n", len(hashes))
		return exitIntegrity
	}
	fmt.Fprintln(w, "hash_stable=true")
	for _, line := range last.metrics.Lines() {
		fmt.Fprintln(w, line)
	}
	if last.metrics.AVSyncMaxUs > maxAVSyncUs {
		fmt.Fprintf(w, "integrity=fail reason=av_sync_exceeds_one_sample\n")
		return exitIntegrity
	}
	if last.metrics.RecoveryRate() < minRecoveryRate {
		fmt.Fprintf(w, "integrity=fail reason=recovery_rate_below_%.2f\n", minRecoveryRate)
		return exitIntegrity
	}
	fmt.Fprintln(w, "integrity=pass")
	return exitOK
}

func exitFor(err error, w io.Writer) int {
	if err == context.Canceled || err == context.DeadlineExceeded {
		fmt.Fprintln(w, "cancelled=true")
		return exitCancelled
	}
	if f, ok := media.AsFault(err); ok {
		fmt.Fprintf(w, "error=%s\n", f)
		if f.Stage == media.StageCalibrate {
			return exitIntegrity
		}
		return exitInvalidMeta
	}
	fmt.Fprintf(w, "error=%v\n", err)
	return 1
}

// runPipeline executes one full pass: fixture → parse → calibrate →
// recover → merge → normalize → render → manifest. All artifacts live
// under TMPDIR and are removed when the pass ends.
func runPipeline(ctx context.Context, opts options) (*runResult, error) {
	tmp, err := os.MkdirTemp("", "archive-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)

	plan, err := fixture.Generate(tmp, fixture.Options{Case: opts.kase, Seed: opts.seed})
	if err != nil {
		return nil, err
	}
	mxfRes, err := ingest.ParseMXF(plan.MXFPath, fixture.VideoTrack)
	if err != nil {
		return nil, err
	}
	mp4Res, err := ingest.ParseMP4(plan.MP4Path, fixture.AudioTrack)
	if err != nil {
		return nil, err
	}
	res := &runResult{faults: append(append([]*media.Fault{}, mxfRes.Faults...), mp4Res.Faults...)}

	// Video: unwrap 33-bit PTS, convert to the unified nanosecond base.
	raw := make([]int64, len(mxfRes.Packets))
	for i, p := range mxfRes.Packets {
		raw[i] = p.PTS
	}
	ticks, err := timebase.Unwrap33(raw, fixture.VideoTrack)
	if err != nil {
		return nil, err
	}
	for i, p := range mxfRes.Packets {
		p.PTS = media.TicksToNS(ticks[i], media.VideoClock)
		p.Duration = media.TicksToNS(p.Duration, media.VideoClock)
	}

	// Audio: recover losses first (parity headers carry local-clock PTS),
	// then fit the drift anchors and correct every packet.
	audio, recovered, lost := recovery.RecoverAll(fixture.AudioTrack, mp4Res.Packets, mp4Res.Parity)
	cal := timebase.Identity()
	if len(mp4Res.Anchors) >= 2 {
		cal, err = timebase.Fit(mp4Res.Anchors)
		if err != nil {
			return nil, err
		}
	}
	for _, p := range audio {
		if !p.Lost {
			p.PTS = cal.Apply(p.PTS)
		}
	}
	var avSyncUs float64
	if n := len(mp4Res.Anchors); n > 2 {
		for _, a := range mp4Res.Anchors[1 : n-1] {
			d := float64(cal.Apply(a.Local) - a.Ref)
			if d < 0 {
				d = -d
			}
			if us := d / 1000; us > avSyncUs {
				avSyncUs = us
			}
		}
	}

	dec, err := normalize.Decide(mxfRes, mp4Res)
	if err != nil {
		return nil, err
	}
	res.decisions = dec.Rules

	merged := timeline.Merge(mxfRes.Packets, audio)
	var video []*media.Packet
	for _, p := range merged {
		if p.Kind == media.KindVideo && !p.Lost {
			video = append(video, p)
		}
	}
	framesDir, err := ingest.SafeJoin(tmp, "frames")
	if err != nil {
		return nil, err
	}
	frames, err := render.Frames(ctx, worker.New(opts.workers), video, dec, framesDir)
	if err != nil {
		return nil, err
	}
	records := make([]report.FrameRecord, len(frames))
	lat := make([]int64, len(frames))
	for i, f := range frames {
		records[i] = report.FrameRecord{
			Name: f.Name, SHA256: f.SHA256, Track: f.Track,
			Seq: f.Seq, PTS: f.PTS, Rules: f.Rules,
		}
		lat[i] = f.LatencyMs
	}
	body, digest := report.Manifest(records)
	// The manifest is only written once every frame succeeded; a failed
	// pass never replaces a completed one.
	if err := os.WriteFile(framesDir+"/manifest.txt", []byte(body), 0o600); err != nil {
		return nil, err
	}
	res.manifest = body
	res.metrics = report.Metrics{
		Frames:        len(frames),
		AVSyncMaxUs:   avSyncUs,
		LossRecovered: recovered,
		LossTotal:     recovered + lost,
		P95Ms:         report.P95(lat),
		ManifestSHA:   digest,
	}
	return res, nil
}
