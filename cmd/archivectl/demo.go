package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"time"

	"archive/internal/manifest"
	"archive/internal/media"
	"archive/internal/scheduler"
	"archive/internal/timeline"
)

// runDemo executes the archive pipeline as a scheduler DAG and prints the
// acceptance metrics. It returns the process exit code.
func runDemo(opts demoOpts, out io.Writer) int {
	ctx := context.Background()
	work, err := os.MkdirTemp("", "archivectl-demo-")
	if err != nil {
		fmt.Fprintln(out, "error:", err)
		return 1
	}
	fmt.Fprintln(out, "work_dir="+work)

	// Heap sampler: the pipeline's memory watermark.
	var heapPeak atomic.Int64
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		var ms runtime.MemStats
		for {
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
			}
			runtime.ReadMemStats(&ms)
			for v := int64(ms.HeapAlloc); v > heapPeak.Load() &&
				!heapPeak.CompareAndSwap(heapPeak.Load(), v); {
			}
		}
	}()

	sched := scheduler.New()
	sched.Add(&scheduler.Node{Name: "manifest", Version: "1", Reentrant: true,
		Params: func() string { return fmt.Sprintf("seed=%d", opts.seed) },
		Run: func(ctx context.Context, _ map[string]any) (any, error) {
			raw, err := json.Marshal(genManifest(opts.seed))
			if err != nil {
				return nil, err
			}
			return manifest.Parse(strings.NewReader(string(raw)))
		}})
	sched.Add(&scheduler.Node{Name: "timeline", Version: "1", Reentrant: true,
		Deps: []string{"manifest"},
		Run: func(ctx context.Context, deps map[string]any) (any, error) {
			m := deps["manifest"].(*manifest.Manifest)
			t := &timelineOut{}
			for i := range m.Tracks {
				rt, err := timeline.Repair(&m.Tracks[i], m.MovieTimescale)
				if err != nil {
					return nil, err
				}
				t.tracks = append(t.tracks, rt)
			}
			d, err := timeline.SyncErrorMicros(t.tracks[0], t.tracks[1])
			if err != nil {
				return nil, err
			}
			t.syncUs = d
			return t, nil
		}})
	sched.Add(&scheduler.Node{Name: "media", Version: "1", Reentrant: true,
		Deps:   []string{"manifest"},
		Params: func() string { return "fixture=" + opts.fixture },
		Run: func(ctx context.Context, deps map[string]any) (any, error) {
			m := deps["manifest"].(*manifest.Manifest)
			p := filepath.Join(work, "demo.mp4")
			if _, err := writeMP4(p, m, opts.seed); err != nil {
				return nil, err
			}
			if opts.fixture == "truncated" {
				fi, _ := os.Stat(p)
				if err := os.Truncate(p, fi.Size()*3/5); err != nil {
					return nil, media.New(media.KindInternal, "demo.media", "", -1, "truncate", err)
				}
			}
			fi, err := os.Stat(p)
			if err != nil {
				return nil, media.New(media.KindInternal, "demo.media", "", -1, "stat", err)
			}
			return &mediaOut{path: p, availByte: fi.Size()}, nil
		}})
	sched.Add(&scheduler.Node{Name: "index", Version: "1", Reentrant: false,
		Deps: []string{"timeline", "media"},
		Run: func(ctx context.Context, deps map[string]any) (any, error) {
			return buildIndex(deps["timeline"].(*timelineOut),
				deps["media"].(*mediaOut), work)
		}})
	sched.Add(&scheduler.Node{Name: "transport", Version: "1", Reentrant: true,
		Deps: []string{"manifest"},
		Params: func() string {
			return fmt.Sprintf("burst=%d,delay=%d", opts.lossBurst, opts.consumerDelayMs)
		},
		Run: func(ctx context.Context, _ map[string]any) (any, error) {
			return runTransport(ctx, opts)
		}})
	sched.Add(&scheduler.Node{Name: "report", Version: "1", Reentrant: true,
		Deps: []string{"timeline", "index", "transport"},
		Run: func(ctx context.Context, deps map[string]any) (any, error) {
			return "ok", nil
		}})

	runErr := sched.Run(ctx)
	return report(out, sched, runErr, opts, &heapPeak)
}
