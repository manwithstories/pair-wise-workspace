package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"bq/internal/manifest"
	"bq/internal/model"
	"bq/internal/pipeline"
	"bq/internal/verify"
)

func runVerify(args []string) error {
	wantValue := map[string]bool{"manifest": true}
	f, err := parseFlags(args, wantValue)
	if err != nil {
		return err
	}
	path, ok := f.values["manifest"]
	if !ok || path == "" {
		return argError("--manifest 为必填项")
	}
	asJSON := f.bools["json"]

	// Strict parse: corrupt or unsupported input becomes a fatal record and
	// exit code 2 before any pipeline work.
	m, parseErrs := manifest.ParseFile(path)
	if len(parseErrs) > 0 {
		return emitFatal(rebuildFatalReport(parseErrs), asJSON)
	}

	// Media files sit beside the manifest; remux/render resolve relative to
	// the manifest's directory.
	outDir := filepath.Dir(path)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	runner := pipeline.NewRunner(outDir)
	res, runErrs := runner.Run(ctx, m)
	if model.HasFatal(runErrs) {
		return emitFatal(res.Report, asJSON)
	}
	if err := writeAndPrint(res, outDir, asJSON); err != nil {
		return err
	}
	switch res.Report.ExitCode() {
	case 1:
		return &cliError{code: 1, msg: "指标超出阈值"}
	case 2:
		return &cliError{code: 2, msg: "不可恢复输入"}
	default:
		return nil
	}
}

func rebuildFatalReport(errs []model.Error) verify.Report {
	model.SortErrors(errs)
	return verify.BuildReport(verify.Metrics{
		ErrorCount: len(errs), FirstSourceOffset: firstOffset(errs),
	}, errs, verify.DefaultThresholds(), 0, 0)
}

func rebuildFatal(res *pipeline.RunResult, errs []model.Error) verify.Report {
	return rebuildFatalReport(errs)
}

func firstOffset(errs []model.Error) int64 {
	for _, e := range errs {
		if e.SourceOffset >= 0 {
			return e.SourceOffset
		}
	}
	return -1
}

func emitFatal(report verify.Report, asJSON bool) error {
	data, err := report.JSON()
	if err != nil {
		return err
	}
	if asJSON {
		fmt.Println(string(data))
	} else {
		fmt.Fprintf(os.Stderr, "不可恢复输入: %d 个致命错误\n", report.Metrics.ErrorCount)
	}
	return &cliError{code: 2, msg: "不可恢复输入"}
}

func dirOf(path string) string {
	return filepath.Dir(path)
}
