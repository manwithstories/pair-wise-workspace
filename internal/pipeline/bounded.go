package pipeline

import (
	"sync"

	"mediapipe/internal/cache"
	"mediapipe/internal/cut"
	"mediapipe/internal/manifest"
)

type segmentOutput struct {
	Segment      manifest.Segment
	Bytes        int64
	Verification cache.Verification
}

// processSegmentsBounded runs at most max segment builders at once. Output
// indices preserve plan order so manifests remain deterministic. Journal and
// cache implementations own their mutexes and atomic file transitions.
func processSegmentsBounded(max int, plans []cut.Plan, fn func(cut.Plan) (segmentOutput, error)) ([]segmentOutput, error) {
	if max <= 0 {
		max = 1
	}
	if max > len(plans) && len(plans) > 0 {
		max = len(plans)
	}
	sem := make(chan struct{}, max)
	out := make([]segmentOutput, len(plans))
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	for i, p := range plans {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, p cut.Plan) {
			defer wg.Done()
			defer func() { <-sem }()
			o, err := fn(p)
			mu.Lock()
			defer mu.Unlock()
			if err != nil && firstErr == nil {
				firstErr = err
				return
			}
			out[i] = o
		}(i, p)
	}
	wg.Wait()
	return out, firstErr
}
