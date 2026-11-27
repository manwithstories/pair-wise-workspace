// Command bcastqc 是广播多声道 PCM/WAV 后期核验与再封装流水线的终端入口。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"bcastqc/internal/model"
	"bcastqc/internal/pipeline"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "demo":
		os.Exit(runDemo(os.Args[2:]))
	case "version":
		fmt.Println("bcastqc demo kernel on go1.25.6")
		os.Exit(0)
	case "-h", "--help", "help":
		usage()
		os.Exit(0)
	default:
		fmt.Fprintf(os.Stderr, "未知子命令 %q\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `bcastqc — 广播多声道 PCM/WAV 核验与再封装流水线

用法:
  bcastqc demo --out <目录> --seed <整数> --tracks <轨道数> \
               --samples <每轨采样数> --bandwidth <档1,档2,...> \
               --faults <all|none|名称,...> [--interrupt-after <阶段步数>]
  bcastqc version

退出码: 0 成功；2 输入/参数无效；3 无法恢复或校验失败
`)
}

// demoArgs 保存 demo 子命令解析后的参数。
type demoArgs struct {
	out            string
	seed           int64
	tracks         int
	samples        int64
	bandwidth      []int64
	faults         pipeline.FaultMask
	interruptAfter int
}

func runDemo(argv []string) int {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	var (
		out        = fs.String("out", "", "演示输出目录")
		seed       = fs.Int64("seed", 41, "夹具随机种子")
		tracks     = fs.Int("tracks", 4, "轨道数")
		samples    = fs.Int64("samples", 48000, "每轨道采样点数")
		bwRaw      = fs.String("bandwidth", "1200000,240000,900000", "带宽计划档位 bit/s，逗号分隔")
		faultsRaw  = fs.String("faults", "all", "故障类型: all|none 或逗号分隔名称")
		interrupt  = fs.Int("interrupt-after", 0, "在指定阶段步数后受控中断（用于演示续跑）")
	)
	if err := fs.Parse(argv); err != nil {
		return 2
	}
	bw, err := parseBandwidth(*bwRaw)
	if err != nil {
		fmt.Fprintf(os.Stderr, "参数错误: %v\n", err)
		return 2
	}
	mask, err := parseFaults(*faultsRaw)
	if err != nil {
		fmt.Fprintf(os.Stderr, "参数错误: %v\n", err)
		return 2
	}
	args := demoArgs{
		out: *out, seed: *seed, tracks: *tracks, samples: *samples,
		bandwidth: bw, faults: mask, interruptAfter: *interrupt,
	}
	if err := args.validate(); err != nil {
		fmt.Fprintf(os.Stderr, "参数错误: %v\n", err)
		return 2
	}

	jobID := fmt.Sprintf("demo-seed%d-t%d-s%d", args.seed, args.tracks, args.samples)
	cfg := pipeline.Config{
		JobID:          jobID,
		OutDir:         args.out,
		Seed:           args.seed,
		Tracks:         args.tracks,
		Samples:        args.samples,
		Bandwidth:      args.bandwidth,
		Faults:         args.faults,
		InterruptAfter: args.interruptAfter,
		ReorderWindow:  4,
		DriftTolerance: 1,
	}

	runner := pipeline.NewRunner(cfg)
	report, err := runner.Run()
	if err != nil {
		if pipeline.IsInterrupted(err) {
			// 受控中断：立即续跑，验证断点恢复与样本数/校验和稳定。
			fmt.Fprintf(os.Stderr, "[interrupt] %v\n", err)
			resumeCfg := cfg
			resumeCfg.InterruptAfter = 0 // 续跑不再中断。
			report, err = pipeline.NewRunner(resumeCfg).Run()
			if err != nil {
				fmt.Fprintf(os.Stderr, "续跑失败: %v\n", err)
				return model.ExitCode(err)
			}
		} else {
			fmt.Fprintf(os.Stderr, "流水线失败: %v\n", err)
			return model.ExitCode(err)
		}
	}
	printReport(report)
	return 0
}

// validate 校验终端参数的合法性与安全边界。
func (a demoArgs) validate() error {
	if a.out == "" {
		return fmt.Errorf("--out 不能为空")
	}
	if a.tracks <= 0 || a.tracks > model.MaxTracks {
		return fmt.Errorf("--tracks 必须在 [1,%d]", model.MaxTracks)
	}
	if a.samples <= 0 {
		return fmt.Errorf("--samples 必须为正")
	}
	if err := model.CheckSampleCount(a.samples); err != nil {
		return err
	}
	if a.interruptAfter < 0 {
		return fmt.Errorf("--interrupt-after 不能为负")
	}
	return nil
}

func parseBandwidth(raw string) ([]int64, error) {
	parts := strings.Split(raw, ",")
	out := make([]int64, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		v, err := strconv.ParseInt(p, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("带宽档位 %q 非法: %w", p, err)
		}
		if v < 0 {
			return nil, fmt.Errorf("带宽档位不能为负: %d", v)
		}
		out = append(out, v)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("带宽计划为空")
	}
	return out, nil
}

func parseFaults(raw string) (pipeline.FaultMask, error) {
	raw = strings.TrimSpace(strings.ToLower(raw))
	if raw == "all" {
		return pipeline.AllFaults(), nil
	}
	if raw == "none" || raw == "" {
		return pipeline.FaultMask{}, nil
	}
	var mask pipeline.FaultMask
	for _, name := range strings.Split(raw, ",") {
		switch strings.TrimSpace(name) {
		case "duplicate":
			mask.Duplicate = true
		case "reorder":
			mask.Reorder = true
		case "burst":
			mask.BurstLoss = true
		case "crc":
			mask.CRCError = true
		case "truncation":
			mask.Truncation = true
		case "riff-overflow":
			mask.RiffOverflow = true
		case "cycle":
			mask.Cycle = true
		default:
			return mask, fmt.Errorf("未知故障类型 %q", name)
		}
	}
	return mask, nil
}

// printReport 输出验收要求的固定字段。
func printReport(r *pipeline.Report) {
	fmt.Println("=== bcastqc demo report ===")
	fmt.Printf("case:                 %s\n", r.Case)
	fmt.Printf("tracks:               %d\n", r.Tracks)
	fmt.Printf("damage_segments:      %d\n", r.DamageSegments)
	fmt.Printf("loc_error_samples:    %d\n", r.LocErrorSamples)
	fmt.Printf("resume_ms:            %d\n", r.ResumeMs)
	fmt.Printf("recovered_samples:    %d\n", r.RecoveredSamples)
	fmt.Printf("rerun_sha_match:      %t\n", r.RerunSHAMatch)
	fmt.Printf("sha256:               %s\n", r.SHA256)
	fmt.Printf("anchor_intervals:     %s\n", anchorLabel(r))
	if r.Manifest != nil {
		fmt.Printf("manifest_entries:     %d\n", len(r.Manifest.Entries))
	}
	// 同时输出一份机器可读 JSON，便于自动化核验。
	if raw, err := json.MarshalIndent(r, "", "  "); err == nil {
		fmt.Println("--- json ---")
		fmt.Println(string(raw))
	}
}

func anchorLabel(r *pipeline.Report) string {
	if r.AnchorNA {
		return "N/A"
	}
	return "present"
}
