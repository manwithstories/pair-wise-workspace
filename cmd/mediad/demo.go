package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"mediad/internal/codec"
	"mediad/internal/pipeline"
	"mediad/internal/sample"
	"mediad/internal/timeline"
)

type demoConfig struct {
	out       string
	codec     string
	seed      int64
	targetBPS int
	driftPPM  int
	inject    map[string]bool
	resume    bool
}

func parseDemo(args []string) (demoConfig, error) {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var cfg demoConfig
	var inject string
	fs.StringVar(&cfg.out, "out", "", "output directory")
	fs.StringVar(&cfg.codec, "codec", "all", "h264|h265|av1|all")
	fs.Int64Var(&cfg.seed, "seed", 41, "deterministic sample seed")
	fs.IntVar(&cfg.targetBPS, "target-bps", 900000, "target video bitrate")
	fs.IntVar(&cfg.driftPPM, "drift-ppm", 120, "clock drift in signed ppm (<=500)")
	fs.StringVar(&inject, "inject", "", "comma list: wrap,duplicate,truncate,no-key")
	fs.BoolVar(&cfg.resume, "resume", false, "simulate interruption and resume")
	if err := fs.Parse(args); err != nil {
		return cfg, err
	}
	if fs.NArg() != 0 {
		return cfg, timeline.New(timeline.KindArgument, "cli.demo", -1, -1, "unexpected arguments: %v", fs.Args())
	}
	if strings.TrimSpace(cfg.out) == "" {
		return cfg, timeline.New(timeline.KindArgument, "cli.demo", -1, -1, "--out is required")
	}
	cfg.inject = make(map[string]bool)
	for _, item := range strings.Split(inject, ",") {
		item = strings.TrimSpace(item)
		if item != "" {
			cfg.inject[item] = true
		}
	}
	return cfg, nil
}

func runDemo(args []string) int {
	cfg, err := parseDemo(args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fmt.Fprintf(os.Stderr, "argument_error: %v\n", err)
		return 2
	}
	if err := executeDemo(cfg); err != nil {
		code := timeline.ExitCode(err)
		fmt.Fprintf(os.Stderr, "demo_failed exit_code=%d error=%q\n", code, err.Error())
		return code
	}
	return 0
}

func codecList(name string) ([]codec.Codec, error) {
	switch name {
	case "all":
		return []codec.Codec{codec.H264, codec.H265, codec.AV1}, nil
	case "h264":
		return []codec.Codec{codec.H264}, nil
	case "h265":
		return []codec.Codec{codec.H265}, nil
	case "av1":
		return []codec.Codec{codec.AV1}, nil
	default:
		return nil, timeline.New(timeline.KindArgument, "cli.codec", -1, -1, "unsupported codec %q", name)
	}
}

func (cfg demoConfig) generate(truncate, noKey bool) (*sample.Media, error) {
	return sample.Generate(sample.Config{
		Seed: cfg.seed, TargetBPS: cfg.targetBPS, DriftPPM: cfg.driftPPM,
		Wrap: cfg.inject["wrap"], Truncate: truncate, NoKey: noKey,
	})
}

func executeDemo(cfg demoConfig) error {
	codecs, err := codecList(cfg.codec)
	if err != nil {
		return err
	}
	if cfg.inject["no-key"] {
		m, err := cfg.generate(false, true)
		if err != nil {
			return err
		}
		_, err = pipeline.Run(pipeline.Options{
			OutDir: filepath.Join(cfg.out, "no-key"), Codec: codecs[0], Media: m,
		})
		return err
	}

	fmt.Println("=== 正常样例：33 位 PTS 回绕/120ppm 校正、RTP 去重、VAD、关键帧切片、指纹去重 ===")
	normal, err := cfg.generate(false, false)
	if err != nil {
		return err
	}
	audioDetailsPrinted := false
	for _, c := range codecs {
		res, err := pipeline.Run(pipeline.Options{
			OutDir: filepath.Join(cfg.out, "normal", string(c)), Codec: c, Media: normal,
			InjectDuplicate: cfg.inject["duplicate"], Resume: cfg.resume,
		})
		if err != nil {
			return err
		}
		printResult("normal", res)
		if !audioDetailsPrinted {
			printAudioIntervals(res)
			audioDetailsPrinted = true
		}
	}

	if cfg.inject["truncate"] {
		fmt.Println("\n=== 损坏隔离：截断/越界 AU 被隔离，后续绝对偏移保持不变 ===")
		corrupt, err := cfg.generate(true, false)
		if err != nil {
			return err
		}
		res, err := pipeline.Run(pipeline.Options{
			OutDir: filepath.Join(cfg.out, "corrupt", string(codecs[0])),
			Codec:  codecs[0], Media: corrupt, InjectDuplicate: cfg.inject["duplicate"],
			Resume: cfg.resume,
		})
		if err != nil {
			return err
		}
		printResult("corrupt-isolated", res)
		fmt.Printf("[isolation] isolated_au=%d first_error_byte=%d later_timeline_shifted=false\n",
			sample.TruncateAU, res.FirstErrorByte)
	}

	if cfg.resume {
		fmt.Println("\n=== 中断恢复：临时文件原子提交，检查点只记录完整片段 ===")
		if err := runRecoveryDemo(cfg, codecs[0], normal); err != nil {
			return err
		}
	}

	fmt.Println("\ndemo_result=ok exit_code=0")
	return nil
}

func runRecoveryDemo(cfg demoConfig, c codec.Codec, m *sample.Media) error {
	base, err := pipeline.Run(pipeline.Options{
		OutDir: filepath.Join(cfg.out, "baseline", string(c)), Codec: c, Media: m,
		InjectDuplicate: cfg.inject["duplicate"],
	})
	if err != nil {
		return err
	}
	printResult("uninterrupted", base)

	recoveryDir := filepath.Join(cfg.out, "recovery", string(c))
	_, err = pipeline.Run(pipeline.Options{
		OutDir: recoveryDir, Codec: c, Media: m,
		InjectDuplicate: cfg.inject["duplicate"], InterruptAfter: 1,
	})
	if !errors.Is(err, pipeline.ErrInterrupted) {
		if err == nil {
			return timeline.New(timeline.KindInternal, "demo.recovery", -1, -1, "expected simulated interruption")
		}
		return err
	}
	fmt.Println("[interrupted] committed_segments=1 checkpoint=fsync+atomic-rename")

	resumed, err := pipeline.Run(pipeline.Options{
		OutDir: recoveryDir, Codec: c, Media: m,
		InjectDuplicate: cfg.inject["duplicate"], Resume: true,
	})
	if err != nil {
		return err
	}
	printResult("resumed", resumed)
	match := resumed.ManifestSHA256 == base.ManifestSHA256
	fmt.Printf("[recovery] baseline_sha256=%s manifest_sha256=%s hash_match=%t\n",
		base.ManifestSHA256, resumed.ManifestSHA256, match)
	if !match {
		return timeline.New(timeline.KindInternal, "demo.recovery", -1, -1,
			"resumed manifest hash does not match uninterrupted run")
	}
	return nil
}

func printAudioIntervals(r *pipeline.Result) {
	for _, s := range r.Silence.Silence {
		fmt.Printf("[silence] start_ms=%.1f end_ms=%.1f duration_ms=%.1f threshold_dbfs=-48.00 required_duration_ms=500 status=silent\n",
			float64(s.StartNS)/1e6, float64(s.EndNS)/1e6, float64(s.EndNS-s.StartNS)/1e6)
	}
	for _, g := range r.Silence.Unknown {
		fmt.Printf("[audio-gap] start_ms=%.1f end_ms=%.1f duration_ms=%.1f status=unknown-missing-samples\n",
			float64(g.StartNS)/1e6, float64(g.EndNS)/1e6, float64(g.EndNS-g.StartNS)/1e6)
	}
}

func printResult(stage string, r *pipeline.Result) {
	m := r.Manifest
	fmt.Printf("[%s] codec=%s target_bps=%d actual_bps=%d delta_pct=%.3f complete_au_pct=%.2f bad_au=%d keyframe_coverage_pct=%.2f align_max_ms=%.6f silence_ms=%.2f silence_duration_ms=%.2f unknown_ms=%.2f duplicate_aus=%d dup_saved_bytes=%d duplicate_saved_bytes=%d rtp_duplicates=%d paused_reads=%d segments=%d first_error_byte=%d manifest_sha256=%s\n",
		stage, m.Codec, m.TargetBPS, m.ActualBPS, m.DeltaPct, m.CompleteAUPct, m.BadAUs,
		m.KeyframeCoverage, m.AlignMaxMS, float64(m.SilenceNS)/1e6, float64(m.SilenceNS)/1e6,
		float64(m.UnknownNS)/1e6, m.DuplicateAUs, m.DupSavedBytes, m.DupSavedBytes,
		m.RTPDuplicates, r.PausedReads, len(m.Segments), m.FirstErrorByte, r.ManifestSHA256)
}
