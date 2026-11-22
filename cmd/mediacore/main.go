// Command mediacore is the terminal entry point for the audio normalisation
// pipeline. It has no GUI dependencies: every feature is a subcommand with
// flags and a one-line machine-readable result.
//
// Subcommands:
//
//	demo    generate a sample MP4, normalise it, optionally stop early
//	resume  continue an interrupted run from its checkpoint
//	verify  re-verify a completed run and print its digest
//	gen     write a synthetic MP4/MOV audio fixture
//	corrupt write one of the four damaged fixtures used by the tests
package main

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"mediacore/internal/codec"
	"mediacore/internal/container"
	"mediacore/internal/engine"
	"mediacore/internal/media"
	"mediacore/internal/store"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		usage()
		return media.ExitInput
	}
	var err error
	switch args[0] {
	case "demo":
		err = cmdDemo(args[1:])
	case "resume":
		err = cmdResume(args[1:])
	case "verify":
		err = cmdVerify(args[1:])
	case "gen":
		err = cmdGen(args[1:])
	case "corrupt":
		err = cmdCorrupt(args[1:])
	case "-h", "--help", "help":
		usage()
		return media.ExitOK
	default:
		err = media.NewError(media.CodeUsage, -1, 0, "unknown subcommand %q", args[0])
	}
	if err == nil {
		return media.ExitOK
	}
	if me, ok := media.AsError(err); ok {
		fmt.Fprintln(os.Stderr, me.Line())
		if me.Message != "" {
			fmt.Fprintf(os.Stderr, "detail: %s\n", me.Message)
		}
		return me.ExitCode()
	}
	fmt.Fprintf(os.Stderr, "error_code=UNCLASSIFIED detail: %v\n", err)
	return media.ExitProcessing
}

func usage() {
	fmt.Fprint(os.Stderr, `usage: mediacore <command> [flags]

  demo    --work DIR [--rate 48000] [--channels stereo] [--loudness -1]
          [--interrupt-after N] [--input-rate 44100] [--seconds 4]
  resume  --work DIR
  verify  --work DIR
  gen     --out FILE [--input-rate 44100] [--channels 2] [--seconds 4]
          [--frame-size 4096] [--ctts-regress]
  corrupt --kind truncated-mdat|invalid-box-length|regressing-ts|unaligned-pcm
          --in FILE --out FILE
`)
}

// --- gen / corrupt commands ---------------------------------------------

func cmdGen(args []string) error {
	fs := flag.NewFlagSet("gen", flag.ContinueOnError)
	out := fs.String("out", "", "output MP4 path")
	rate := fs.Int("input-rate", 44100, "source sample rate")
	channels := fs.Int("channels", 2, "source channel count")
	seconds := fs.Float64("seconds", 4, "duration in seconds")
	frameSize := fs.Int("frame-size", 4096, "frames per container sample")
	ctts := fs.Bool("ctts-regress", false, "insert a regressing composition offset")
	odd := fs.Bool("odd-size", false, "make the first sample an odd byte count")
	if err := fs.Parse(args); err != nil {
		return media.NewError(media.CodeUsage, -1, 0, "%v", err)
	}
	if *out == "" {
		return media.NewError(media.CodeUsage, -1, 0, "--out is required")
	}
	spec := container.FixtureSpec{
		Rate:      *rate,
		Channels:  *channels,
		Frames:    int64(*seconds * float64(*rate)),
		FrameSize: *frameSize,
		CTTSTrick: *ctts,
		OddSize:   *odd,
	}
	if err := container.WriteFixture(*out, spec); err != nil {
		return err
	}
	st, err := os.Stat(*out)
	if err != nil {
		return err
	}
	fmt.Printf("wrote=%s bytes=%d rate_in=%d channels=%d frames=%d\n", *out, st.Size(), *rate, *channels, spec.Frames)
	return nil
}

func cmdCorrupt(args []string) error {
	fs := flag.NewFlagSet("corrupt", flag.ContinueOnError)
	kind := fs.String("kind", "", "damage kind")
	in := fs.String("in", "", "source MP4")
	out := fs.String("out", "", "damaged MP4")
	if err := fs.Parse(args); err != nil {
		return media.NewError(media.CodeUsage, -1, 0, "%v", err)
	}
	if *kind == "" || *in == "" || *out == "" {
		return media.NewError(media.CodeUsage, -1, 0, "--kind, --in and --out are required")
	}
	data, err := os.ReadFile(*in)
	if err != nil {
		return media.NewError(media.CodeUsage, -1, 0, "read %s: %v", *in, err).Wrap(err)
	}
	off, err := corrupt(*kind, data, *out, *in)
	if err != nil {
		return err
	}
	fmt.Printf("wrote=%s kind=%s expected_offset=%d bytes=%d\n", *out, *kind, off, mustSize(*out))
	return nil
}

// corrupt applies one damage kind and returns the byte offset at which the
// first anomaly is located.
func corrupt(kind string, data []byte, out, in string) (int64, error) {
	switch kind {
	case "truncated-mdat":
		idx := indexOfBox(data, "mdat")
		if idx < 0 {
			return 0, media.NewError(media.CodeUsage, -1, 0, "input has no mdat")
		}
		payload := idx + 8
		cut := payload + (len(data)-payload)/2
		if err := os.WriteFile(out, data[:cut], 0o644); err != nil {
			return 0, media.NewError(media.CodeUsage, -1, 0, "write: %v", err).Wrap(err)
		}
		// The first byte that should exist but does not is the new EOF.
		return int64(cut), nil
	case "invalid-box-length":
		idx := indexOfBox(data, "moov")
		if idx < 0 {
			return 0, media.NewError(media.CodeUsage, -1, 0, "input has no moov")
		}
		buf := append([]byte(nil), data...)
		binary.BigEndian.PutUint32(buf[idx:], uint32(len(data)+4096))
		if err := os.WriteFile(out, buf, 0o644); err != nil {
			return 0, media.NewError(media.CodeUsage, -1, 0, "write: %v", err).Wrap(err)
		}
		return int64(idx), nil
	case "regressing-ts":
		return 0, media.NewError(media.CodeUsage, -1, 0,
			"regressing-ts is produced by 'gen --ctts-regress', not by mutation")
	case "unaligned-pcm":
		return 0, media.NewError(media.CodeUsage, -1, 0,
			"unaligned-pcm is produced by 'gen --odd-size', not by mutation")
	default:
		return 0, media.NewError(media.CodeUsage, -1, 0, "unknown damage kind %q", kind)
	}
}

func indexOfBox(data []byte, tag string) int {
	for i := 4; i+4 <= len(data); i++ {
		if string(data[i:i+4]) == tag {
			// Require a plausible size field immediately before it.
			size := binary.BigEndian.Uint32(data[i-4:])
			if int(size) >= 8 && i-4+int(size) <= len(data) {
				return i - 4
			}
		}
	}
	return -1
}

func mustSize(path string) int64 {
	st, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return st.Size()
}

func absPath(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return p, nil
	}
	return abs, nil
}

// --- demo / resume / verify --------------------------------------------

// commonFlags are shared by demo, resume and verify.
type commonFlags struct {
	work      string
	rate      int
	channels  string
	loudness  float64
	blockMs   int
	interrupt int
	inputRate int
	seconds   float64
	input     string
}

func addCommon(fs *flag.FlagSet, c *commonFlags) {
	fs.StringVar(&c.work, "work", "", "work directory")
	fs.IntVar(&c.rate, "rate", 48000, "output sample rate (44100 or 48000)")
	fs.StringVar(&c.channels, "channels", "stereo", "output layout: mono or stereo")
	fs.Float64Var(&c.loudness, "loudness", -1, "peak ceiling in dBFS, in [-60, 0)")
	fs.IntVar(&c.blockMs, "block-ms", 100, "output block duration in milliseconds")
	fs.IntVar(&c.interrupt, "interrupt-after", 0, "stop after N completed blocks")
	fs.StringVar(&c.input, "input", "", "input MP4 (default: generated fixture)")
	fs.IntVar(&c.inputRate, "input-rate", 44100, "generated fixture rate")
	fs.Float64Var(&c.seconds, "seconds", 4, "generated fixture duration")
}

func (c *commonFlags) config() (engine.Config, error) {
	ch := 0
	switch c.channels {
	case "mono", "1":
		ch = 1
	case "stereo", "2":
		ch = 2
	default:
		return engine.Config{}, media.NewError(media.CodeChannelsInvalid, -1, 0,
			"--channels must be mono or stereo, got %q", c.channels)
	}
	if c.blockMs <= 0 || c.blockMs > 1000 {
		return engine.Config{}, media.NewError(media.CodeBlockTooLong, -1, 0,
			"--block-ms %d must be in (0, 1000]", c.blockMs)
	}
	frames := c.rate * c.blockMs / 1000
	if frames <= 0 {
		return engine.Config{}, media.NewError(media.CodeBlockTooLong, -1, 0,
			"--block-ms %d yields no frames at %d Hz", c.blockMs, c.rate)
	}
	return engine.Config{
		OutputRate:     c.rate,
		OutputChannels: ch,
		LoudnessDBFS:   c.loudness,
		BlockFrames:    frames,
	}, nil
}

func cmdDemo(args []string) error {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	var c commonFlags
	addCommon(fs, &c)
	if err := fs.Parse(args); err != nil {
		return media.NewError(media.CodeUsage, -1, 0, "%v", err)
	}
	if c.work == "" {
		return media.NewError(media.CodeUsage, -1, 0, "--work is required")
	}
	cfg, err := c.config()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(c.work, 0o755); err != nil {
		return media.NewError(media.CodeAtomicWrite, -1, 0, "create work dir: %v", err).Wrap(err)
	}
	input := c.input
	generated := false
	if input == "" {
		input = filepath.Join(c.work, "input.mp4")
		spec := container.FixtureSpec{
			Rate:      c.inputRate,
			Channels:  2,
			Frames:    int64(c.seconds * float64(c.inputRate)),
			FrameSize: 4096,
		}
		if err := container.WriteFixture(input, spec); err != nil {
			return err
		}
		generated = true
	}
	return runPipeline(pipelineRequest{
		mode:           "demo",
		work:           c.work,
		input:          input,
		cfg:            cfg,
		interruptAfter: c.interrupt,
		fresh:          true,
		generated:      generated,
	})
}

func cmdResume(args []string) error {
	fs := flag.NewFlagSet("resume", flag.ContinueOnError)
	var c commonFlags
	addCommon(fs, &c)
	if err := fs.Parse(args); err != nil {
		return media.NewError(media.CodeUsage, -1, 0, "%v", err)
	}
	if c.work == "" {
		return media.NewError(media.CodeUsage, -1, 0, "--work is required")
	}
	cfg, err := c.config()
	if err != nil {
		return err
	}
	cp, err := store.Load(c.work)
	if err != nil {
		return err
	}
	if cp == nil {
		return media.NewError(media.CodeUsage, -1, 0, "no checkpoint in %s", c.work)
	}
	input := cp.InputPath
	if _, err := os.Stat(input); err != nil {
		return media.NewError(media.CodeUsage, -1, 0,
			"checkpoint input %s is no longer readable: %v", input, err).Wrap(err)
	}
	// On resume the configuration comes from the checkpoint; explicit
	// flags may confirm but never override, so a typo cannot silently
	// produce a different output.
	cfg = engine.Config{
		OutputRate:     cp.OutputRate,
		OutputChannels: cp.OutputChannels,
		LoudnessDBFS:   cp.LoudnessDBFS,
		BlockFrames:    cp.BlockFrames,
	}
	return runPipeline(pipelineRequest{
		mode:       "resume",
		work:       c.work,
		input:      input,
		cfg:        cfg,
		resumeFrom: cp.CompletedBlocks,
	})
}

func cmdVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	var c commonFlags
	addCommon(fs, &c)
	if err := fs.Parse(args); err != nil {
		return media.NewError(media.CodeUsage, -1, 0, "%v", err)
	}
	if c.work == "" {
		return media.NewError(media.CodeUsage, -1, 0, "--work is required")
	}
	cp, err := store.Load(c.work)
	if err != nil {
		return err
	}
	if cp == nil || !cp.Complete {
		return media.NewError(media.CodeOutputVerification, -1, 0, "no complete checkpoint in %s", c.work)
	}
	sha, err := store.Assemble(c.work, cp.Blocks, cp.CanonicalSHA256)
	if err != nil {
		return err
	}
	fmt.Printf("verify=ok samples_out=%d blocks=%d sha256=%s\n", outputSamples(cp), len(cp.Blocks), sha)
	return nil
}

func outputSamples(cp *store.Checkpoint) int64 {
	var n int64
	for _, b := range cp.Blocks {
		n += int64(b.Frames) * int64(cp.OutputChannels)
	}
	return n
}

// --- shared pipeline driver --------------------------------------------

// pipelineRequest is the normalised form of a subcommand invocation.
type pipelineRequest struct {
	mode           string
	work           string
	input          string
	cfg            engine.Config
	interruptAfter int
	resumeFrom     int
	fresh          bool
	generated      bool
}

func runPipeline(req pipelineRequest) error {
	started := time.Now()
	if err := engine.CheckInsideWorkDir(req.work, req.work); err != nil {
		return err
	}
	adapter := container.NewMP4(container.DefaultOptions())
	src, f, err := adapter.OpenFile(req.input)
	if err != nil {
		return recordFailure(req, err, started)
	}
	defer f.Close()

	sha, size, err := fingerprint(req.input)
	if err != nil {
		return recordFailure(req, err, started)
	}
	if err := engine.CheckInsideWorkDir(req.work, req.work); err != nil {
		return recordFailure(req, err, started)
	}
	res, err := engine.Run(engine.Options{
		WorkDir:        req.work,
		Config:         req.cfg,
		Source:         src,
		Codecs:         codec.Default(),
		InputPath:      req.input,
		InputSHA:       sha,
		InputSize:      size,
		Mode:           req.mode,
		InterruptAfter: req.interruptAfter,
		Fresh:          req.fresh,
	})
	if err != nil {
		return recordFailure(req, err, started)
	}
	if err := recordSuccess(req, res, sha, size, started); err != nil {
		return err
	}
	render(req, res)
	return nil
}

func fingerprint(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, media.NewError(media.CodeUsage, -1, 0, "open %s: %v", path, err).Wrap(err)
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, media.NewError(media.CodeUsage, -1, 0, "hash %s: %v", path, err).Wrap(err)
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func render(req pipelineRequest, res *engine.Result) {
	s := res.Stats
	if res.Interrupted {
		fmt.Printf("checkpoint_block=%d interrupted=1 blocks_done=%d total_blocks=%d sha256=%s\n",
			s.Blocks, s.Blocks, res.Checkpoint.TotalBlocks, s.SHA256)
		return
	}
	if req.mode == "resume" {
		fmt.Printf("checkpoint_block=%d resumed_blocks=%d recovery_ms=%d samples_in=%d samples_out=%d rate_out=%d channels=%d peak_dbfs=%.2f sha256=%s\n",
			req.resumeFrom, s.ResumedBlocks, s.RecoveryMS, s.SamplesIn, s.SamplesOut,
			req.cfg.OutputRate, req.cfg.OutputChannels, s.PeakDBFS, s.SHA256)
		return
	}
	fmt.Printf("samples_in=%d samples_out=%d rate_out=%d channels=%d peak_dbfs=%.2f sha256=%s\n",
		s.SamplesIn, s.SamplesOut, req.cfg.OutputRate, req.cfg.OutputChannels, s.PeakDBFS, s.SHA256)
	fmt.Printf("blocks=%d silent_blocks=%d clipped_blocks=%d limited_blocks=%d rtp_first=%d rtp_last=%d rtp_verified=%t\n",
		s.Blocks, s.SilentBlocks, s.ClippedBlocks, s.LimitedBlocks, s.RTPFirst, s.RTPLast, s.RTPVerified)
}

func recordSuccess(req pipelineRequest, res *engine.Result, sha string, size int64, started time.Time) error {
	cp := res.Checkpoint
	p := &store.Provenance{
		Mode:             req.mode,
		InputPath:        req.input,
		InputSHA256:      sha,
		InputBytes:       size,
		ContainerVersion: cp.ContainerVersion,
		CodecVersion:     cp.CodecVersion,
		ConfigDigest:     cp.ConfigDigest,
		Params: store.Params{
			Rate: req.cfg.OutputRate, Channels: req.cfg.OutputChannels,
			LoudnessDBFS: req.cfg.LoudnessDBFS, BlockFrames: req.cfg.BlockFrames,
			InterruptAfter: req.interruptAfter,
		},
		Output: store.OutputSummary{
			Path: store.OutputPath(req.work), SHA256: res.Stats.SHA256,
			SamplesIn: res.Stats.SamplesIn, FramesOut: res.Stats.FramesOut,
			SamplesOut: res.Stats.SamplesOut, Blocks: res.Stats.Blocks,
			PeakDBFS: res.Stats.PeakDBFS, TotalBytes: res.Stats.TotalBytes,
		},
		ResumedFromBlock: req.resumeFrom,
		RecoveryMS:       time.Since(started).Milliseconds(),
		ExitCode:         media.ExitOK,
	}
	if res.Interrupted {
		p.ExitCode = media.ExitOK
		p.Note = "interrupted after " + strconv.Itoa(res.Stats.Blocks) + " blocks"
	}
	return store.Append(req.work, p)
}

func recordFailure(req pipelineRequest, err error, started time.Time) error {
	code := media.ExitCodeOf(err)
	me, _ := media.AsError(err)
	p := &store.Provenance{
		Mode:      req.mode,
		InputPath: req.input,
		Params: store.Params{
			Rate: req.cfg.OutputRate, Channels: req.cfg.OutputChannels,
			LoudnessDBFS: req.cfg.LoudnessDBFS, BlockFrames: req.cfg.BlockFrames,
			InterruptAfter: req.interruptAfter,
		},
		Errors:     store.ErrorsFrom([]error{err}),
		RecoveryMS: time.Since(started).Milliseconds(),
		ExitCode:   code,
	}
	if me != nil {
		p.Output.SHA256 = ""
	}
	// Best effort: a provenance write failure must not mask the original
	// error or change its exit code.
	_ = store.Append(req.work, p)
	return err
}
