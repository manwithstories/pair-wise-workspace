package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"bq/internal/fixture"
	"bq/internal/model"
	"bq/internal/pipeline"
)

func runDemo(args []string) error {
	wantValue := map[string]bool{
		"minutes": true, "seed": true, "inject-loss": true,
		"anchor-conflicts": true, "out": true,
	}
	f, err := parseFlags(args, wantValue)
	if err != nil {
		return err
	}
	minutes, err := requireInt(f, "minutes", 30)
	if err != nil {
		return err
	}
	seed, err := requireUint(f, "seed", 41)
	if err != nil {
		return err
	}
	out, ok := f.values["out"]
	if !ok || out == "" {
		return argError("--out 为必填项")
	}
	loss, err := optionalFloat(f, "inject-loss", 0.02)
	if err != nil {
		return err
	}
	conflicts, err := requireInt(f, "anchor-conflicts", 0)
	if err != nil {
		return err
	}
	if err := validateOutputDir(out); err != nil {
		return err
	}

	opts := fixture.Options{
		Seed: seed, DurationS: int64(minutes) * 60, LossRate: loss,
		InjectWrap: f.bools["inject-wrap"], InjectDup: f.bools["inject-dup"],
		InjectTrunc: f.bools["inject-trunc"], AnchorConflicts: conflicts,
		PacketCadenceS: 1, GroupSize: 5,
	}
	// The acceptance demo always exercises the full anomaly surface even when
	// individual flags are omitted, matching the reference command.
	if !f.bools["inject-wrap"] && !f.bools["inject-dup"] && !f.bools["inject-trunc"] {
		opts.InjectWrap = true
		opts.InjectDup = true
		opts.InjectTrunc = true
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	fmt.Fprintf(os.Stderr, "生成夹具: %d 分钟, seed=%d, 丢包率=%.2f%%, 输出=%s\n",
		minutes, seed, loss*100, out)
	m, err := pipeline.Generate(ctx, opts, out)
	if err != nil {
		return &cliError{code: 2, msg: "夹具生成失败: " + err.Error()}
	}

	runner := pipeline.NewRunner(out)
	res, _ := runner.Run(ctx, m)
	if err := writeAndPrint(res, out, true); err != nil {
		return err
	}
	// The reference demo also emits the corruption sample next to the main
	// manifest so the damage cases are reviewable in one directory.
	if err := writeDamageSamples(ctx, out, opts); err != nil {
		fmt.Fprintf(os.Stderr, "警告: 损坏样例生成失败: %v\n", err)
	}
	if res.Report.ExitCode() == 2 && model.HasFatal(res.AllErrors) {
		return &cliError{code: 2, msg: "存在不可恢复输入，详见报告"}
	}
	if res.Report.ExitCode() == 1 {
		return &cliError{code: 1, msg: "指标超出阈值，详见报告"}
	}
	return nil
}

func writeAndPrint(res *pipeline.RunResult, outDir string, asJSON bool) error {
	reportPath := filepath.Join(outDir, "report.json")
	data, err := res.Report.JSON()
	if err != nil {
		return err
	}
	if err := os.WriteFile(reportPath, data, 0o644); err != nil {
		return fmt.Errorf("写报告失败: %w", err)
	}
	if asJSON {
		// Print the metrics block at top level for the acceptance contract.
		top, err := res.Report.MetricsJSON()
		if err != nil {
			return err
		}
		fmt.Println(string(top))
	}
	fmt.Fprintf(os.Stderr, "报告已写入 %s (status=%s)\n", reportPath, res.Report.Status)
	return nil
}

func validateOutputDir(out string) error {
	// Keep every artifact beneath an explicit, non-root directory.
	clean := filepath.Clean(out)
	if clean == "." || clean == string(filepath.Separator) || clean == "" {
		return argError("--out 不能是根目录或当前目录占位")
	}
	if !filepath.IsAbs(clean) {
		abs, err := filepath.Abs(clean)
		if err != nil {
			return argError("无法解析输出目录: %v", err)
		}
		clean = abs
	}
	if err := os.MkdirAll(clean, 0o755); err != nil {
		return &cliError{code: 2, msg: "无法创建输出目录: " + err.Error()}
	}
	return nil
}

func requireInt(f *flags, key string, def int) (int, error) {
	v, ok := f.values[key]
	if !ok {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, argError("--%s 需要非负整数, 得到 %q", key, v)
	}
	return n, nil
}

func requireUint(f *flags, key string, def uint64) (uint64, error) {
	v, ok := f.values[key]
	if !ok {
		return def, nil
	}
	n, err := strconv.ParseUint(v, 10, 64)
	if err != nil {
		return 0, argError("--%s 需要无符号整数, 得到 %q", key, v)
	}
	return n, nil
}

func optionalFloat(f *flags, key string, def float64) (float64, error) {
	v, ok := f.values[key]
	if !ok {
		return def, nil
	}
	n, err := strconv.ParseFloat(v, 64)
	if err != nil || n < 0 || n > 1 {
		return 0, argError("--%s 需要 [0,1] 之间的小数, 得到 %q", key, v)
	}
	return n, nil
}
