package batch

import (
	"os"
	"path/filepath"
	"sort"
	"sync"

	"loudcheck/internal/loudness"
)

const StreamFrames = 16384

type Status string

const (
	StatusPass      Status = "PASS"
	StatusFail      Status = "FAIL"
	StatusReadError Status = "READ_ERROR"
)

type FileResult struct {
	Path       string                 `json:"path"`
	Status     Status                 `json:"status"`
	Result     loudness.EngineResult `json:"result"`
	ReadError  string                 `json:"read_error,omitempty"`
	DataOffset int64                  `json:"data_offset,omitempty"`
	Declared   int64                  `json:"declared,omitempty"`
}

type Summary struct {
	Total     int `json:"total"`
	Pass      int `json:"pass"`
	Fail      int `json:"fail"`
	ReadError int `json:"read_error"`
}

type Profile struct {
	Name             string
	TargetLUFS       float64
	ToleranceLU      float64
	MaxTruePeakDBTP  float64
}

func EBUProfile() Profile {
	return Profile{
		Name:            "ebu-r128",
		TargetLUFS:      -23,
		ToleranceLU:     0.5,
		MaxTruePeakDBTP: -1,
	}
}

func (p Profile) Evaluate(result loudness.EngineResult) (Status, string) {
	if !result.Defined {
		return StatusFail, "integrated loudness undefined (silence or too short)"
	}
	if result.IntegratedLUFS > p.TargetLUFS+p.ToleranceLU ||
		result.IntegratedLUFS < p.TargetLUFS-p.ToleranceLU {
		return StatusFail, "integrated loudness outside target window"
	}
	if result.TruePeakDBTP > p.MaxTruePeakDBTP {
		return StatusFail, "true peak above limit"
	}
	return StatusPass, "within EBU R128 target and true peak limit"
}

func discoverWAV(root string) ([]string, error) {
	paths := make([]string, 0)
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && filepath.Ext(path) == ".wav" {
			paths = append(paths, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	return paths, nil
}

func Run(root string, workers int, profile Profile) ([]FileResult, Summary, error) {
	paths, err := discoverWAV(root)
	if err != nil {
		return nil, Summary{}, err
	}
	if workers < 1 {
		workers = 1
	}
	results := make([]FileResult, len(paths))
	jobs := make(chan int)
	var wait sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for index := range jobs {
				results[index] = analyzeOne(paths[index], profile)
			}
		}()
	}
	for index := range paths {
		jobs <- index
	}
	close(jobs)
	wait.Wait()
	summary := Summary{Total: len(results)}
	for _, result := range results {
		switch result.Status {
		case StatusPass:
			summary.Pass++
		case StatusFail:
			summary.Fail++
		case StatusReadError:
			summary.ReadError++
		}
	}
	return results, summary, nil
}

func analyzeOne(path string, profile Profile) FileResult {
	result := FileResult{Path: path}
	file, err := os.Open(path)
	if err != nil {
		result.Status = StatusReadError
		result.ReadError = err.Error()
		return result
	}
	defer file.Close()
	metric, reader, err := loudness.MeasureStream(file, StreamFrames)
	if err != nil {
		result.Status = StatusReadError
		result.ReadError = err.Error()
		if reader != nil {
			result.DataOffset = reader.Header().DataOffset
			result.Declared = reader.Header().DataLength
		}
		return result
	}
	result.Result = metric
	status, _ := profile.Evaluate(metric)
	result.Status = status
	return result
}

func AnalyzeSingle(path string, profile Profile) FileResult {
	return analyzeOne(path, profile)
}
