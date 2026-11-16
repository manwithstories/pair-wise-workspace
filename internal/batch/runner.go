// Package batch runs measurements concurrently over files or
// directories, aggregates per-file errors without stopping the batch,
// and keeps output ordering stable (input order, not completion order).
package batch

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/sync/errgroup"

	"loudcheck/internal/meter"
	"loudcheck/internal/report"
)

// Outcome is the aggregate of a batch run.
type Outcome struct {
	Results []*meter.Result
	Errors  []report.FileError
}

// Collect expands inputs (files or directories) into a sorted,
// de-duplicated list of WAV files. Directories are walked
// non-recursively; unknown inputs are an error.
func Collect(inputs []string) ([]string, error) {
	var files []string
	seen := map[string]bool{}
	for _, in := range inputs {
		st, err := os.Stat(in)
		if err != nil {
			return nil, fmt.Errorf("input %s: %w", in, err)
		}
		if st.IsDir() {
			entries, err := os.ReadDir(in)
			if err != nil {
				return nil, fmt.Errorf("input %s: %w", in, err)
			}
			var names []string
			for _, e := range entries {
				if !e.IsDir() && strings.HasSuffix(strings.ToLower(e.Name()), ".wav") {
					names = append(names, filepath.Join(in, e.Name()))
				}
			}
			sort.Strings(names)
			for _, n := range names {
				if !seen[n] {
					seen[n] = true
					files = append(files, n)
				}
			}
			continue
		}
		if !seen[in] {
			seen[in] = true
			files = append(files, in)
		}
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no WAV files found in the given inputs")
	}
	return files, nil
}

// Run measures every file with at most workers concurrent measurements.
// Per-file failures are recorded and do not stop the batch; result
// order matches the input file order.
func Run(files []string, opts meter.Options, workers int) Outcome {
	if workers < 1 {
		workers = 1
	}
	type slot struct {
		res *meter.Result
		err error
	}
	slots := make([]slot, len(files))
	var g errgroup.Group
	g.SetLimit(workers)
	for i, f := range files {
		i, f := i, f
		g.Go(func() error {
			res, err := meter.MeasureFile(f, opts)
			slots[i] = slot{res, err}
			return nil // errors are aggregated, not propagated
		})
	}
	g.Wait()

	var out Outcome
	for i, s := range slots {
		if s.err != nil {
			out.Errors = append(out.Errors, report.FileError{File: files[i], Err: s.err})
		} else {
			out.Results = append(out.Results, s.res)
		}
	}
	return out
}
