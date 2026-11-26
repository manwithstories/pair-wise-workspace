package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"

	"aircheck/internal/pipeline"
)

// caseSummary 每个样例的验收摘要（JSON 行输出）。
type caseSummary struct {
	Case            string  `json:"case"`
	TargetKbps      float64 `json:"target_kbps"`
	ActualKbps      float64 `json:"actual_kbps"`
	RateDeltaPct    float64 `json:"rate_delta_pct"`
	PSNRMeanDB      float64 `json:"psnr_mean_db"`
	PSNRMinDB       float64 `json:"psnr_min_db"`
	AlignMaxMs      float64 `json:"align_max_ms"`
	GapFrames       int     `json:"gap_frames"`
	StrictExit      int     `json:"strict_exit"`
	ResumeHashMatch bool    `json:"resume_hash_match"`
}

var allCases = []string{"normal", "wrap_drift", "badhash", "gap", "truncated"}

func cmdDemo(args []string) int {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	caseName := fs.String("case", "all", "样例：all|normal|wrap_drift|badhash|gap|truncated")
	kbps := fs.Float64("target-kbps", 900, "目标码率 kbps")
	out := fs.String("out", "", "输出目录（样例与结果仅写入此目录）")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if *out == "" {
		fmt.Fprintln(os.Stderr, "demo: --out is required")
		return 1
	}
	cases := allCases
	if *caseName != "all" {
		cases = []string{*caseName}
	}
	ok := true
	for _, c := range cases {
		sum, err := runDemoCase(c, *kbps, *out)
		if err != nil {
			fmt.Fprintf(os.Stderr, "case %s error: %v\n", c, err)
			ok = false
			continue
		}
		line, _ := json.Marshal(sum)
		fmt.Println(string(line))
		if msg := checkAcceptance(sum); msg != "" {
			fmt.Fprintf(os.Stderr, "case %s FAILED: %s\n", c, msg)
			ok = false
		}
	}
	if !ok {
		return 1
	}
	return 0
}

// runDemoCase 生成样例并执行：完整运行、截断中断、续跑比对。
func runDemoCase(caseName string, kbps float64, outRoot string) (*caseSummary, error) {
	fixDir := filepath.Join(outRoot, "fixtures", caseName)
	manifest, srcRoot, err := genCaseFixtures(caseName, fixDir)
	if err != nil {
		return nil, err
	}
	sum := &caseSummary{Case: caseName, TargetKbps: kbps}
	base := pipeline.Options{ManifestPath: manifest, SourceRoot: srcRoot,
		TargetKbps: kbps, Width: fixtureW, Height: fixtureH}
	if caseName == "badhash" {
		strict := base
		strict.Strict = true
		strict.OutDir = filepath.Join(outRoot, "cases", caseName, "strict")
		_, err := pipeline.Run(context.Background(), strict)
		var ee *pipeline.ExitError
		switch {
		case errors.As(err, &ee):
			sum.StrictExit = ee.Code
		case err != nil:
			sum.StrictExit = 1
		}
	}
	full := base
	full.OutDir = filepath.Join(outRoot, "cases", caseName, "full")
	fullRes, err := pipeline.Run(context.Background(), full)
	if err != nil {
		return nil, fmt.Errorf("full run: %w", err)
	}
	part := base
	part.OutDir = filepath.Join(outRoot, "cases", caseName, "resumed")
	part.InterruptAfter = fullRes.Frames / 2
	if _, err := pipeline.Run(context.Background(), part); !errors.Is(err, pipeline.ErrInterrupted) {
		return nil, fmt.Errorf("interrupt run: %w", err)
	}
	resumed := part
	resumed.InterruptAfter = 0
	resumed.Resume = true
	resRes, err := pipeline.Run(context.Background(), resumed)
	if err != nil {
		return nil, fmt.Errorf("resume run: %w", err)
	}
	sum.ResumeHashMatch = resRes.OutputSHA == fullRes.OutputSHA
	sum.ActualKbps = round2(fullRes.Rate.ActualKbps)
	sum.RateDeltaPct = round2(fullRes.Rate.DeltaPct)
	sum.PSNRMeanDB = round2(fullRes.Rate.PSNRMeanDB)
	sum.PSNRMinDB = round2(fullRes.Rate.PSNRMinDB)
	sum.AlignMaxMs = round3(fullRes.AlignMaxMs)
	sum.GapFrames = fullRes.GapFrames
	return sum, nil
}

// checkAcceptance 按样例类型判定验收标准，返回空串表示通过。
func checkAcceptance(s *caseSummary) string {
	if !s.ResumeHashMatch {
		return "resume output hash mismatch"
	}
	switch s.Case {
	case "normal":
		if math.Abs(s.RateDeltaPct) > 8 {
			return fmt.Sprintf("rate delta %.2f%% exceeds 8%%", s.RateDeltaPct)
		}
		if s.PSNRMeanDB < 28 {
			return fmt.Sprintf("psnr mean %.2f dB below 28 dB", s.PSNRMeanDB)
		}
		if s.AlignMaxMs > 1 {
			return fmt.Sprintf("align %.3f ms exceeds 1 ms", s.AlignMaxMs)
		}
	case "wrap_drift":
		if s.AlignMaxMs > 1 {
			return fmt.Sprintf("align %.3f ms exceeds 1 ms after drift correction", s.AlignMaxMs)
		}
	case "badhash":
		if s.StrictExit != 2 {
			return fmt.Sprintf("strict exit %d, want 2", s.StrictExit)
		}
	case "gap":
		if s.GapFrames != 3 {
			return fmt.Sprintf("gap frames %d, want 3", s.GapFrames)
		}
	case "truncated":
		// 续跑哈希一致即通过（上面已校验）
	}
	return ""
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }
func round3(v float64) float64 { return math.Round(v*1000) / 1000 }
