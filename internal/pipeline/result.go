// Package pipeline wires local discovery, bounded workers, checkpoints, and
// deterministic report merging.
package pipeline

import (
	"context"
	"sort"

	"mediascan/internal/analyze"
	"mediascan/internal/media"
	"mediascan/internal/parser"
	"mediascan/internal/store"
)

// Job is one local asset task.
type Job struct {
	Index int
	Path  string
	Task  store.Task
}

// FileResult is the durable per-task analysis result. It is JSON-safe and is
// stored verbatim inside the task checkpoint.
type FileResult struct {
	Task      store.Task               `json:"task"`
	Index     int                      `json:"index"`
	Path      string                   `json:"path"`
	Name      string                   `json:"name"`
	SHA256    string                   `json:"sha256"`
	Size      int64                    `json:"size"`
	Streams   []analyze.Stream         `json:"streams"`
	Keyframes []analyze.KeyframeReport `json:"keyframes"`
	Faults    []media.Fault            `json:"faults"`
}

// AnalyzePath opens, hashes, parses, and analyzes one local asset.
func AnalyzePath(_ context.Context, job Job) (*FileResult, error) {
	src, err := parser.OpenSource(job.Path)
	if err != nil {
		return nil, err
	}
	defer src.Close()

	pf := parser.ParseFile(src)
	streams := analyze.AnalyzeStreams(pf, job.Index)
	keyframes := analyze.AnalyzeKeyframes(pf, job.Index)

	return &FileResult{
		Task:      job.Task,
		Index:     job.Index,
		Path:      job.Path,
		Name:      src.Name,
		SHA256:    src.SHA256,
		Size:      src.Size,
		Streams:   streams,
		Keyframes: keyframes,
		Faults:    pf.Faults,
	}, nil
}

// sortedJobs returns jobs in deterministic index/path order.
func sortedJobs(jobs []Job) []Job {
	out := append([]Job(nil), jobs...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Index != out[j].Index {
			return out[i].Index < out[j].Index
		}
		return out[i].Path < out[j].Path
	})
	return out
}
