package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"review/internal/fault"
	"review/internal/metrics"
)

// 退出码：0 通过；1 指标超限；2 参数错误；3 不可恢复输入或事务失败。
const (
	exitPass     = 0
	exitMetrics  = 1
	exitArgs     = 2
	exitUnusable = 3
)

func main() {
	os.Exit(run())
}

func run() int {
	fs := flag.NewFlagSet("review", flag.ContinueOnError)
	demo := fs.Bool("demo", false, "run demo pipeline with generated fixtures")
	injectLoss := fs.Int("inject-loss", 0, "drop N packets (one per group, recoverable)")
	truncateTail := fs.Bool("truncate-tail", false, "truncate tail of camB hi last segment")
	dropIndex := fs.Int("drop-index", 0, "drop N sidx entries from camC hi segment 2")
	repeat := fs.Int("repeat", 1, "repeat whole pipeline N times to verify hash consistency")
	workdir := fs.String("workdir", "", "working directory (default: auto temp)")
	if err := fs.Parse(os.Args[1:]); err != nil {
		return exitArgs
	}
	if !*demo {
		fmt.Fprintln(os.Stderr, "usage: review --demo [--inject-loss N] [--truncate-tail] [--drop-index N] [--repeat N] [--workdir DIR]")
		return exitArgs
	}
	if *injectLoss < 0 || *dropIndex < 0 || *repeat < 1 {
		fmt.Fprintln(os.Stderr, "invalid arguments: loss/drop must be >= 0, repeat >= 1")
		return exitArgs
	}
	root := *workdir
	if root == "" {
		var err error
		root, err = os.MkdirTemp("", "review-demo-")
		if err != nil {
			fmt.Fprintln(os.Stderr, "workdir:", err)
			return exitUnusable
		}
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "workdir:", err)
		return exitUnusable
	}
	fmt.Printf("workdir=%s\n", root)

	opts := options{
		demo: *demo, injectLoss: *injectLoss, truncateTail: *truncateTail,
		dropIndex: *dropIndex, repeat: *repeat,
	}
	var last *metrics.Report
	var faults []*fault.Error
	digests := map[string]bool{}
	for i := 0; i < *repeat; i++ {
		dir := filepath.Join(root, fmt.Sprintf("run%d", i))
		rep, fl, err := runPipeline(dir, opts)
		faults = fl
		if err != nil {
			fmt.Fprintf(os.Stderr, "pipeline failed: %v\n", err)
			return exitUnusable
		}
		digests[rep.SHA256] = true
		last = rep
		fmt.Printf("run %d: frames=%d sha256=%s\n", i, rep.Frames, rep.SHA256)
	}
	last.RepeatHashEqual = len(digests) == 1
	fmt.Println("--- faults (recoverable chain) ---")
	for _, f := range faults {
		fmt.Println(f)
	}
	fmt.Println("--- acceptance report ---")
	last.Print(os.Stdout)
	if !last.Pass() {
		return exitMetrics
	}
	return exitPass
}
