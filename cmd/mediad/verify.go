package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"mediad/internal/fixture"
	segmenthash "mediad/internal/hash"
)

func runVerify(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	fs.SetOutput(stderr)
	input := fs.String("input", "", "batch directory to verify")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *input == "" {
		fmt.Fprintln(stderr, "mediad: --input is required")
		return exitUsage
	}
	abs, err := filepath.Abs(*input)
	if err != nil {
		fmt.Fprintf(stderr, "mediad: %v\n", err)
		return exitUsage
	}
	info, err := os.Stat(abs)
	if err != nil || !info.IsDir() {
		fmt.Fprintf(stderr, "mediad: input %s is not a directory\n", abs)
		return exitUsage
	}
	report, err := loadReport(abs)
	if err != nil {
		fmt.Fprintf(stderr, "mediad: %v\n", err)
		return exitUsage
	}
	total, matched, problems := 0, 0, 0
	verified := map[string]bool{}
	for _, r := range report.Cases {
		for _, rec := range r.SegmentHashes {
			if verified[rec.Path] {
				continue
			}
			verified[rec.Path] = true
			total++
			path, err := fixture.SafeJoin(abs, rec.Path)
			if err != nil {
				fmt.Fprintf(stdout, "verify segment=%s error=%q\n", rec.Path, err.Error())
				problems++
				continue
			}
			data, err := os.ReadFile(path)
			if err != nil {
				fmt.Fprintf(stdout, "verify segment=%s error=%q\n", rec.Path, err.Error())
				problems++
				continue
			}
			got := string(segmenthash.Bytes(data))
			if got != rec.SHA256 {
				fmt.Fprintf(stdout, "verify segment=%s expected=%s got=%s\n", rec.Path, rec.SHA256, got)
				problems++
				continue
			}
			matched++
		}
	}
	fmt.Fprintf(stdout, "verify sha256_match=%d/%d problems=%d\n", matched, total, problems)
	if total == 0 {
		fmt.Fprintln(stdout, "verify: no segment evidence found")
		return exitUsage
	}
	if matched != total || problems != 0 {
		return exitFailure
	}
	return exitOK
}
