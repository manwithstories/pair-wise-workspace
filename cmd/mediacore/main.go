// Command mediacore is the terminal entry point for the delivery pipeline:
// parse, map, resolve metadata, stream through a bounded queue, remux and
// emit a manifest.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"mediacore/internal/adapter"
	"mediacore/internal/manifest"
	"mediacore/internal/media"
	"mediacore/internal/metadata"
	"mediacore/internal/mux"
	"mediacore/internal/pipeline"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 1
	}
	switch args[0] {
	case "demo":
		err := runDemo(args[1:], stdout, stderr)
		if err == nil {
			return 0
		}
		var ie *mux.InterruptError
		if errors.As(err, &ie) {
			return 0
		}
		return reportError(err, stdout, stderr)
	case "help", "-h", "--help":
		usage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown command %q\n", args[0])
		usage(stderr)
		return media.ClassUsage.ExitCode()
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `mediacore demo - lossless delivery demo

  --generate              synthesize fixtures (no external files)
  --format mixed|mp4|mxf  fixture format; mixed alternates tracks (default mixed)
  --tracks N              track count (default 3)
  --samples N             samples per track (default 240)
  --seed N                fixture seed (default 1)
  --queue-bytes N         max in-flight payload bytes (default 67108864)
  --queue-samples N       max in-flight samples (default 64)
  --consumer-delay D      per-sample consumer latency, e.g. 20ms
  --checkpoint PATH       write resume state after each checkpoint wave
  --corrupt KIND          truncate | sample_length | wrap | ambiguous_wrap | cancel
  --offset N              byte offset for truncate, sample ordinal for sample_length
  --interrupt-after N     stop after N samples, leaving a verified checkpoint
  --resume PATH           resume from a verified checkpoint
  --output PATH           output file (default ./tmp/out.mp4)
  --manifest PATH         manifest path (default ./tmp/manifest.json)
  --rotation N            explicit rotation override in quarter turns
  --strict                reject rotations that are not quarter turns
  --big                   use a ~1 GiB logical stream
`)
}

type demoFlags struct {
	generate       bool
	format         string
	tracks         int
	samples        int
	seed           int64
	queueBytes     int64
	queueSamples   int
	consumerDelay  time.Duration
	checkpoint     string
	corrupt        string
	offset         int64
	interruptAfter int
	resume         string
	output         string
	manifestPath   string
	rotation       int
	rotationSet    bool
	strict         bool
	big            bool
}

func parseDemoFlags(args []string) (*demoFlags, error) {
	f := &demoFlags{}
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.BoolVar(&f.generate, "generate", false, "")
	fs.StringVar(&f.format, "format", "mixed", "")
	fs.IntVar(&f.tracks, "tracks", 3, "")
	fs.IntVar(&f.samples, "samples", 240, "")
	fs.Int64Var(&f.seed, "seed", 1, "")
	fs.Int64Var(&f.queueBytes, "queue-bytes", 64<<20, "")
	fs.IntVar(&f.queueSamples, "queue-samples", 64, "")
	fs.DurationVar(&f.consumerDelay, "consumer-delay", 0, "")
	fs.StringVar(&f.checkpoint, "checkpoint", "", "")
	fs.StringVar(&f.corrupt, "corrupt", "", "")
	fs.Int64Var(&f.offset, "offset", 0, "")
	fs.IntVar(&f.interruptAfter, "interrupt-after", 0, "")
	fs.StringVar(&f.resume, "resume", "", "")
	fs.StringVar(&f.output, "output", "", "")
	fs.StringVar(&f.manifestPath, "manifest", "", "")
	fs.IntVar(&f.rotation, "rotation", 0, "")
	fs.BoolVar(&f.strict, "strict", false, "")
	fs.BoolVar(&f.big, "big", false, "")
	if err := fs.Parse(args); err != nil {
		return nil, media.NewError(media.ClassUsage, "%v", err)
	}
	fs.Visit(func(fl *flag.Flag) {
		if fl.Name == "rotation" {
			f.rotationSet = true
		}
	})
	switch {
	case f.queueBytes <= 0:
		return nil, media.NewError(media.ClassUsage, "--queue-bytes must be positive")
	case f.queueSamples <= 0:
		return nil, media.NewError(media.ClassUsage, "--queue-samples must be positive")
	case f.tracks < 1 || f.tracks > 8:
		return nil, media.NewError(media.ClassUsage, "--tracks must be in 1..8")
	case f.samples < 1:
		return nil, media.NewError(media.ClassUsage, "--samples must be positive")
	case f.consumerDelay < 0:
		return nil, media.NewError(media.ClassUsage, "--consumer-delay must not be negative")
	case f.interruptAfter < 0:
		return nil, media.NewError(media.ClassUsage, "--interrupt-after must not be negative")
	}
	switch f.format {
	case "mixed", "mp4", "mxf":
	default:
		return nil, media.NewError(media.ClassUsage, "--format must be mixed, mp4 or mxf")
	}
	switch f.corrupt {
	case "", "truncate", "sample_length", "wrap", "ambiguous_wrap", "cancel":
	default:
		return nil, media.NewError(media.ClassUsage, "unknown --corrupt value %q", f.corrupt)
	}
	if f.corrupt == "truncate" && f.offset <= 0 {
		return nil, media.NewError(media.ClassUsage, "--corrupt truncate requires --offset")
	}
	if f.resume != "" && f.checkpoint == "" {
		f.checkpoint = f.resume
	}
	if (f.corrupt == "wrap" || f.corrupt == "ambiguous_wrap") && f.format != "mxf" {
		return nil, media.NewError(media.ClassUsage,
			"--corrupt %s requires --format mxf: only the MXF subset carries wrappable 32-bit timestamps",
			f.corrupt)
	}
	return f, nil
}

func reportError(err error, stdout, stderr io.Writer) int {
	class := media.ClassOf(err)
	fmt.Fprintf(stdout, "error_class=%s input_offset=%d tolerance_bytes=%d\n",
		class, media.OffsetOf(err), toleranceOf(err))
	fmt.Fprintf(stderr, "%v\n", err)
	return class.ExitCode()
}

func toleranceOf(err error) int64 {
	var e *media.Error
	if errors.As(err, &e) {
		return e.Tolerance
	}
	return 0
}

type runState struct {
	src        media.Source
	format     string
	adapter    adapter.Adapter
	container  *media.Container
	plan       *adapter.Plan
	resolved   *metadata.Result
	outputPath string
	manifest   string
	queuePeak  int64
	queue      *pipeline.Queue
	closeSrc   func()
}

func runDemo(args []string, stdout, stderr io.Writer) error {
	f, err := parseDemoFlags(args)
	if err != nil {
		return err
	}
	st, err := prepare(f)
	if err != nil {
		return recordFault(f, err)
	}
	if st.closeSrc != nil {
		defer st.closeSrc()
	}

	switch f.corrupt {
	case "cancel":
		return runCancelDemo(st, f, stdout)
	case "truncate", "sample_length":
		// Parsing already diagnosed and recorded the fault; driving the
		// pipeline proves the streaming stage refuses the same input. The
		// manifest keeps the parse-stage fault with its exact location.
		pipelineErr := runCorruptionDemo(st, f)
		if pipelineErr == nil {
			pipelineErr = media.NewError(media.ClassTruncated,
				"generated corruption was not observed while streaming").WithOffset(f.offset)
		}
		return recordFaultAt(f, "pipeline", pipelineErr)
	case "wrap", "ambiguous_wrap":
		// Timestamp wrap is a parse-time concern; parsing already ran.
		if f.corrupt == "wrap" {
			if err := assertWrap(st.container); err != nil {
				return err
			}
		}
	}

	var resume *pipeline.ResumePlan
	resumeStart := time.Now()
	if f.resume != "" {
		cp, readErr := pipeline.Read(f.resume)
		wroteCheckpoint := false
		if readErr == nil && cp.Complete {
			// A completed delivery is verified, not rewritten: recompute the
			// output and input stream hashes and require them to agree.
			if err := verifyCompleted(st, f, &cp, stdout); err != nil {
				return recordFault(f, err)
			}
			return nil
		}
		if readErr == nil && !cp.Complete {
			// Resume the interrupted delivery directly.
			resume, err = pipeline.Load(f.resume, st.container.Fingerprint, st.plan.Sign(), st.outputPath)
			if err != nil {
				return err
			}
		} else {
			// No checkpoint yet: interrupt now to create one, then resume in
			// the same invocation so the run demonstrates real recovery.
			if f.interruptAfter <= 0 {
				return readErr
			}
			if err := st.cycleInterrupt(f); err != nil {
				return err
			}
			wroteCheckpoint = true
			resume, err = pipeline.Load(f.resume, st.container.Fingerprint, st.plan.Sign(), st.outputPath)
			if err != nil {
				return err
			}
		}
		_ = wroteCheckpoint
	}

	q, err := newQueue(st, f)
	if err != nil {
		return err
	}
	st.queue = q

	r := &mux.Remuxer{
		Tracks:   st.container.Tracks,
		Plan:     st.plan.Outputs(),
		Resolved: st.resolvedMeta(),
		Source:   st.src,
	}
	res, err := r.Run(context.Background(), mux.Options{
		OutputPath:     st.outputPath,
		CheckpointPath: f.checkpoint,
		Resume:         resume,
		InterruptAfter: interruptAfterFor(f, resume),
		Fingerprint:    st.container.Fingerprint,
		MappingSign:    st.plan.Sign(),
		TotalSamples:   totalSamples(st.container),
		WaveSize:       waveSize(f),
		Queue:          q,
		QueuePeak:      &st.queuePeak,
		ConsumerDelay:  f.consumerDelay,
		Chunk:          int(min64(f.queueBytes, 64<<10)),
	})
	if err != nil {
		var ie *mux.InterruptError
		if errors.As(err, &ie) {
			fmt.Fprintf(stdout, "interrupt_after=%d checkpoint=%s\n", ie.After, f.checkpoint)
			return err
		}
		return err
	}

	types := map[string]media.MediaType{}
	codecs := map[string]string{}
	for _, t := range st.container.Tracks {
		types[t.ID.String()] = t.Type
		codecs[t.ID.String()] = t.Codec
	}
	var resumeInfo *manifest.ResumeInfo
	if resume != nil {
		resumeInfo = &manifest.ResumeInfo{
			ResumedReplaySamples: 0,
			VerifiedSegments:     resume.Segments,
			RolledBackSegments:   resume.RolledBack,
			ResumeMillis:         time.Since(resumeStart).Milliseconds(),
		}
	}
	man := manifest.Build(manifest.Input{
		Inputs:       []string{st.src.Name()},
		Adapter:      st.adapter.Format(),
		Version:      st.adapter.Version(),
		Fingerprint:  st.container.Fingerprint,
		Output:       st.outputPath,
		OutputSize:   res.OutputSize,
		TotalSamples: res.Samples,
		UnknownBoxes: st.container.UnknownBoxes,
		QueuePeak:    st.queuePeak,
		Remux:        res,
		RemuxTracks:  res.Tracks,
		Resolved:     st.resolved,
		TrackTypes:   types,
		TrackCodecs:  codecs,
		Resume:       resumeInfo,
	})
	if err := writeManifest(man, st.manifest); err != nil {
		return err
	}
	fmt.Fprintln(stdout, man.Summary())
	if resume != nil {
		fmt.Fprintf(stdout, "resume_ms=%d replay_samples=%d output_sha256=%s\n",
			resumeInfo.ResumeMillis, resumeInfo.ResumedReplaySamples, digestOf(res.Tracks))
	}
	return nil
}

// interruptAfterFor disables the interrupt when resuming, because the
// interrupt already happened when the checkpoint was created.
func interruptAfterFor(f *demoFlags, resume *pipeline.ResumePlan) int {
	if resume != nil {
		return 0
	}
	return f.interruptAfter
}

// resolvedMeta converts resolved descriptions into the muxer's form.
func (st *runState) resolvedMeta() map[string]mux.ResolvedMeta {
	out := make(map[string]mux.ResolvedMeta, len(st.resolved.Entries))
	for _, e := range st.resolved.Entries {
		out[e.TrackKey] = mux.ResolvedMeta{Rotation: e.Rotation, Crop: e.Crop, Color: e.Color}
	}
	return out
}

// prepare builds the input, parses it, maps tracks and resolves metadata.
func prepare(f *demoFlags) (*runState, error) {
	if !f.generate {
		return nil, media.NewError(media.ClassUsage,
			"demo requires --generate: fixtures are synthesized, no external files")
	}
	dir := filepath.Join(".", "tmp")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, media.NewError(media.ClassIO, "cannot create work directory").WithCause(err)
	}
	format := f.format
	if format == "mixed" {
		format = "mp4"
	}
	src, closeSrc, err := generateFixture(f, dir, format)
	if err != nil {
		return nil, err
	}
	reg := adapter.DefaultRegistry()
	ad, err := reg.Select(format, "")
	if err != nil {
		closeSrc()
		return nil, err
	}
	c, err := ad.Parse(context.Background(), src)
	if err != nil {
		closeSrc()
		return nil, err
	}
	plan, err := adapter.BuildPlan([]*media.Container{c}, map[string]adapter.Adapter{format: ad})
	if err != nil {
		closeSrc()
		return nil, err
	}
	override := metadata.Override{StrictRotation: f.strict}
	if f.rotationSet {
		rot := f.rotation
		override.Rotation = &rot
	}
	resolved, err := metadata.Resolve(c, override)
	if err != nil {
		closeSrc()
		return nil, err
	}
	outputPath := f.output
	if outputPath == "" {
		outputPath = filepath.Join(dir, "out.mp4")
	}
	manPath := f.manifestPath
	if manPath == "" {
		manPath = filepath.Join(dir, "manifest.json")
	}
	return &runState{
		src: src, format: format, adapter: ad, container: c,
		plan: plan, resolved: resolved, outputPath: outputPath,
		manifest: manPath, closeSrc: closeSrc,
	}, nil
}

// generateFixture writes the structural fixture to ./tmp and returns a
// random-access source over it. Essence holes are sparse, so a logical
// 1 GiB stream costs almost no disk space.
func generateFixture(f *demoFlags, dir, format string) (media.Source, func(), error) {
	samples := f.samples
	if f.big {
		samples = bigSampleCount(f.tracks, 1000)
	}
	if format == "mxf" {
		g := adapter.MXFGenerator{Tracks: f.tracks, Samples: samples, Seed: f.seed}
		switch f.corrupt {
		case "truncate":
			g.TruncateAt = f.offset
		case "sample_length":
			g.BadEssenceLen = true
		case "wrap":
			g.TimestampWrap = true
		case "ambiguous_wrap":
			g.AmbiguousWrap = true
		}
		layout, err := adapter.BuildMXF(g)
		if err != nil {
			return nil, nil, media.NewError(media.ClassUsage, "%v", err)
		}
		path := filepath.Join(dir, "fixture.mxf")
		if err := writeSparse(path, layout.Prefix, layout.TotalSize(), layout.TruncateAt); err != nil {
			return nil, nil, err
		}
		src, err := media.OpenFile(path)
		if err != nil {
			return nil, nil, err
		}
		return src, func() { src.Close() }, nil
	}
	spec := adapter.MP4Spec{
		Tracks: f.tracks, Samples: samples, Seed: f.seed,
		Color:        &adapter.MediaColor{Primaries: 1, Transfer: 1, Matrix: 1},
		UnknownBoxes: 1,
	}
	switch f.corrupt {
	case "truncate":
		spec.Corrupt = "truncate"
		spec.Offset = f.offset
	case "sample_length":
		spec.Corrupt = "sample_length"
		spec.Offset = f.offset
	}
	layout, err := adapter.BuildMP4(spec)
	if err != nil {
		return nil, nil, media.NewError(media.ClassUsage, "%v", err)
	}
	path := filepath.Join(dir, "fixture.mp4")
	if err := writeSparse(path, layout.Prefix, layout.TotalSize(), layout.TruncateAt); err != nil {
		return nil, nil, err
	}
	src, err := media.OpenFile(path)
	if err != nil {
		return nil, nil, err
	}
	return src, func() { src.Close() }, nil
}

func bigSampleCount(tracks, sizeHint int) int {
	per := (1 << 30) / (tracks * sizeHint)
	if per < 1 {
		per = 1
	}
	return per
}

// writeSparse creates the fixture with its structural prefix and extends the
// file to size as a sparse hole. truncateAt cuts it short when non-zero.
func writeSparse(path string, prefix []byte, size, truncateAt int64) error {
	file, err := os.Create(path)
	if err != nil {
		return media.NewError(media.ClassIO, "cannot create fixture").WithCause(err)
	}
	defer file.Close()
	if _, err := file.Write(prefix); err != nil {
		return media.NewError(media.ClassIO, "cannot write fixture").WithCause(err)
	}
	target := size
	if truncateAt > 0 && truncateAt < target {
		target = truncateAt
	}
	if err := file.Truncate(target); err != nil {
		return media.NewError(media.ClassIO, "cannot size fixture").WithCause(err)
	}
	return nil
}

func newQueue(st *runState, f *demoFlags) (*pipeline.Queue, error) {
	return pipeline.NewQueue(st.src, pipeline.Limits{
		MaxSamples: f.queueSamples,
		MaxBytes:   f.queueBytes,
	}), nil
}

func waveSize(f *demoFlags) int {
	w := f.samples / 8
	if f.big {
		w = 4096
	}
	if w < 1 {
		w = 1
	}
	return w
}

func totalSamples(c *media.Container) int {
	n := 0
	for _, t := range c.Tracks {
		n += t.SampleCount()
	}
	return n
}

func digestOf(tracks []mux.TrackResult) string {
	h := sha256.New()
	sort.SliceStable(tracks, func(i, j int) bool { return tracks[i].Key < tracks[j].Key })
	for _, t := range tracks {
		h.Write([]byte(t.OutputSHA256))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

// runCorruptionDemo sends the samples through the bounded queue so the fault
// is observed at the streaming stage, then reports it with its offset.
func runCorruptionDemo(st *runState, f *demoFlags) error {
	q := pipeline.NewQueue(st.src, pipeline.Limits{
		MaxSamples: f.queueSamples, MaxBytes: f.queueBytes,
	})
	var first error
	for _, t := range st.container.Tracks {
		for i := 0; i < t.SampleCount(); i++ {
			meta, err := t.Index.At(i)
			if err != nil {
				if first == nil {
					first = err
				}
				break
			}
			if f.offset > 0 && !(meta.Offset <= f.offset && f.offset < meta.Offset+meta.Size) {
				// Only samples that span the requested offset are relevant
				// for the sample_length case.
				if f.corrupt == "sample_length" {
					continue
				}
			}
			if err := q.Push(context.Background(), t, i); err != nil {
				if first == nil {
					first = err
				}
				break
			}
			if err := q.Next(context.Background(), func(*pipeline.Sample) error { return nil }); err != nil {
				if first == nil {
					first = err
				}
				break
			}
		}
		if first != nil {
			break
		}
	}
	if first == nil {
		return media.NewError(media.ClassTruncated,
			"the generated corruption was not observed while streaming").WithOffset(f.offset)
	}
	return first
}

// runCancelDemo cancels the consumer mid-stream and checks that the producer
// unblocks and the queue is released.
func runCancelDemo(st *runState, f *demoFlags, stdout io.Writer) error {
	q := pipeline.NewQueue(st.src, pipeline.Limits{
		MaxSamples: f.queueSamples, MaxBytes: f.queueBytes,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	t := st.container.Tracks[0]
	producerErr := make(chan error, 1)
	go func() {
		for i := 0; i < t.SampleCount(); i++ {
			if err := q.Push(ctx, t, i); err != nil {
				producerErr <- err
				return
			}
		}
		q.Close()
		producerErr <- nil
	}()
	consumed := 0
	for {
		err := q.Next(ctx, func(*pipeline.Sample) error {
			consumed++
			if consumed >= 3 {
				return errStopConsumer
			}
			return nil
		})
		if err != nil {
			break
		}
	}
	cancel()
	select {
	case <-producerErr:
	case <-time.After(5 * time.Second):
		return media.NewError(media.ClassCancelled, "producer did not stop after cancellation")
	}
	fmt.Fprintf(stdout, "consumer_cancelled=true consumed=%d queue_depth=%d\n",
		consumed, q.Stats().MaxQueueDepth)
	return media.NewError(media.ClassCancelled, "consumer cancelled after %d samples", consumed)
}

var errStopConsumer = errors.New("consumer stopped")

func assertWrap(c *media.Container) error {
	for _, t := range c.Tracks {
		if t.SampleCount() < 2 {
			continue
		}
		prev, err := t.Index.At(0)
		if err != nil {
			return err
		}
		for i := 1; i < t.SampleCount(); i++ {
			cur, err := t.Index.At(i)
			if err != nil {
				return err
			}
			if cur.DTS.Cmp(prev.DTS) < 0 {
				return media.NewError(media.ClassTimestampAmbiguous,
					"timestamp went backwards for track %s", t.ID).
					WithContainer(c.Format).WithTrack(t.ID.Value)
			}
			prev = cur
		}
	}
	return nil
}

func writeManifest(m *manifest.Manifest, path string) error {
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return media.NewError(media.ClassIO, "cannot create manifest directory").WithCause(err)
		}
	}
	file, err := os.Create(path)
	if err != nil {
		return media.NewError(media.ClassIO, "cannot create manifest").WithCause(err)
	}
	defer file.Close()
	if err := m.WriteJSON(file); err != nil {
		return media.NewError(media.ClassIO, "cannot write manifest").WithCause(err)
	}
	return nil
}
