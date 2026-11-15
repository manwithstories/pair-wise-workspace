// Package batch runs a bounded worker pool over WAV files while preserving
// input order in the output.
package batch

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"loudness/internal/loudness"
	"loudness/internal/report"
)

type Options struct {
	Dir       string
	Jobs      int
	Target    float64
	Ceiling   float64
	Tolerance float64
}

type Result struct {
	Rows    []report.Row
	Summary report.Summary
}

type job struct {
	index int
	path  string
}

// Run measures all .wav files directly under dir. Each worker owns one
// Processor and therefore reuses filters, block buffers and interpolator
// state without sharing mutable state with other workers.
func Run(opts Options) (Result, error) {
	files, err := listWAVs(opts.Dir)
	if err != nil {
		return Result{}, err
	}
	if len(files) == 0 {
		return Result{}, fmt.Errorf("no .wav files in %s", opts.Dir)
	}
	jobs := opts.Jobs
	if jobs <= 0 {
		jobs = 1
	}
	if jobs > len(files) {
		jobs = len(files)
	}
	results := make([]report.Row, len(files))
	work := make(chan job)
	var wg sync.WaitGroup
	wg.Add(jobs)
	for i := 0; i < jobs; i++ {
		go func() {
			defer wg.Done()
			processor := loudness.NewProcessor()
			for item := range work {
				m, measureErr := processor.ProcessFile(item.path)
				name := filepath.Base(item.path)
				if measureErr != nil {
					results[item.index] = report.ErrorRow(name, measureErr)
					continue
				}
				results[item.index] = report.Judge(name, m, opts.Target, opts.Ceiling, opts.Tolerance)
			}
		}()
	}
	for i, path := range files {
		work <- job{index: i, path: path}
	}
	close(work)
	wg.Wait()

	out := Result{Rows: results}
	out.Summary.Total = len(results)
	for _, row := range results {
		switch row.Verdict {
		case report.VerdictPass:
			out.Summary.Pass++
		case report.VerdictFail:
			out.Summary.Fail++
		default:
			out.Summary.Error++
		}
	}
	return out, nil
}

func listWAVs(dir string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if strings.EqualFold(filepath.Ext(d.Name()), ".wav") {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	return files, nil
}
