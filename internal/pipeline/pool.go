package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"mediascan/internal/store"
)

// Outcome is one processed or resumed task.
type Outcome struct {
	Job                 Job
	Result              *FileResult
	Skipped             bool
	CommittedNow        bool
	FingerprintMismatch bool
	Err                 error
}

// Options configures the bounded worker pool.
type Options struct {
	Workers     int
	Store       *store.Store
	Analyze     func(context.Context, Job) (*FileResult, error)
	OnCommitted func(taskKey string)
}

// Produce sends sorted jobs to workers and remains cancellation-aware.
func Produce(ctx context.Context, jobs []Job, out chan<- Job) error {
	defer close(out)
	for _, job := range sortedJobs(jobs) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case out <- job:
		}
	}
	return nil
}

// Run executes jobs with at most Options.Workers concurrent analyzers. It
// deduplicates identical task keys so retries cannot create duplicate outputs.
// Canceled jobs are not reported as completed outcomes.
func Run(ctx context.Context, jobs []Job, opts Options) []Outcome {
	jobs = dedupeJobs(jobs)
	workers := opts.Workers
	if workers < 1 {
		workers = 1
	}
	if workers > 16 {
		workers = 16
	}
	analyze := opts.Analyze
	if analyze == nil {
		analyze = AnalyzePath
	}

	jobCh := make(chan Job)
	outCh := make(chan Outcome, len(jobs))
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range jobCh {
				if ctx.Err() != nil {
					return
				}
				outcome := processJob(ctx, job, opts, analyze)
				if outcome.Err != nil || outcome.Result != nil || outcome.Skipped {
					select {
					case outCh <- outcome:
					case <-ctx.Done():
						return
					}
				}
			}
		}()
	}

	go func() { _ = Produce(ctx, jobs, jobCh) }()
	go func() {
		wg.Wait()
		close(outCh)
	}()

	var outcomes []Outcome
	for o := range outCh {
		outcomes = append(outcomes, o)
	}
	sort.SliceStable(outcomes, func(i, j int) bool { return outcomes[i].Job.Index < outcomes[j].Job.Index })
	return outcomes
}

func dedupeJobs(jobs []Job) []Job {
	seen := map[string]bool{}
	var out []Job
	for _, j := range sortedJobs(jobs) {
		key := j.Task.Key
		if key == "" {
			key = fmt.Sprintf("path:%s:%d", j.Path, j.Index)
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, j)
	}
	return out
}

func processJob(ctx context.Context, job Job, opts Options, analyze func(context.Context, Job) (*FileResult, error)) Outcome {
	o := Outcome{Job: job}
	if opts.Store != nil {
		cp, found, err := opts.Store.Resolve(job.Task)
		if err != nil {
			o.Err = err
			o.FingerprintMismatch = store.IsMismatch(err)
			return o
		}
		if found && cp != nil {
			var cached FileResult
			if err := json.Unmarshal(cp.Result, &cached); err != nil {
				o.Err = fmt.Errorf("decode checkpoint for %s: %w", job.Task.Key, err)
				return o
			}
			if cached.Task.Key == "" {
				cached.Task = cp.Task
			}
			o.Result, o.Skipped = &cached, true
			return o
		}
	}

	result, err := analyze(ctx, job)
	if err != nil {
		o.Err = err
		return o
	}
	if ctx.Err() != nil {
		o.Err = ctx.Err()
		return o
	}
	if opts.Store != nil {
		if _, err := opts.Store.SaveCommitted(job.Task, result); err != nil {
			o.Err = err
			return o
		}
		o.CommittedNow = true
		if opts.OnCommitted != nil {
			opts.OnCommitted(job.Task.Key)
		}
	}
	o.Result = result
	return o
}

// Discover lists regular .mp4/.mov files below root without following symlinks.
func Discover(root string) ([]string, error) {
	absRoot, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return nil, err
	}
	var files []string
	err = filepath.WalkDir(absRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil // never follow a link outside the workspace
		}
		ext := strings.ToLower(filepath.Ext(d.Name()))
		if ext != ".mp4" && ext != ".mov" {
			return nil
		}
		rel, err := filepath.Rel(absRoot, path)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("%w: %s", ErrPathEscape, path)
		}
		files = append(files, path)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	return files, nil
}

// ErrPathEscape marks a path that resolves outside its permitted root.
var ErrPathEscape = errors.New("path escapes workspace")

// SafePath joins name to root and rejects absolute names and parent escape.
func SafePath(root, name string) (string, error) {
	if filepath.IsAbs(name) {
		return "", fmt.Errorf("%w: absolute name %q", ErrPathEscape, name)
	}
	clean := filepath.Clean(name)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: %q", ErrPathEscape, name)
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	target := filepath.Join(absRoot, clean)
	rel, err := filepath.Rel(absRoot, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: %q", ErrPathEscape, name)
	}
	return target, nil
}
