package pipeline

import (
	"context"
	"sort"
	"sync"

	"mediad/internal/bitstream"
	"mediad/internal/color"
	"mediad/internal/fault"
	"mediad/internal/manifest"
	"mediad/internal/timeline"
	"mediad/internal/video"
)

type frameJob struct {
	trackID string
	fr      manifest.Frame
	data    []byte
}

// runVideo checks the track timeline, then streams frames through
// verify/scan into a bounded job channel consumed by proxy workers.
// Corrupt frames are isolated; later valid frames keep flowing.
func runVideo(ctx context.Context, clock timeline.Clock, t *manifest.Track,
	opts Options, res *Result, faults *fault.List) error {
	fps := *t.FPS
	step, err := clock.StepPTS(fps)
	if err != nil {
		return err
	}
	frames := sortFrames(t.Frames)
	refs := make([]timeline.FrameRef, len(frames))
	for i, f := range frames {
		refs[i] = timeline.FrameRef{Seq: f.Seq, PTS: f.PTS}
	}
	badSeq := map[int64]bool{}
	for _, d := range clock.Check(refs, step) {
		kind := fault.KindMissingFrame
		if d.Regression {
			kind = fault.KindBadTimestamp
			badSeq[d.Seq] = true
		}
		faults.Add(fault.Fault{Track: t.ID, Frame: d.Seq, Kind: kind,
			Recoverable: true, Detail: "timeline discontinuity"})
	}
	keys := make([]bool, len(frames))
	for i, f := range frames {
		keys[i] = f.Keyframe
	}
	for _, idx := range bitstream.KeyframeViolations(keys, opts.MaxKeyGap) {
		faults.Add(fault.Fault{Track: t.ID, Frame: frames[idx].Seq,
			Kind: fault.KindKeyframeInterval, Recoverable: true,
			Detail: "keyframe interval exceeded"})
	}
	tag, action := color.ApplySRGB(t.ColorTag)
	res.ColorActions[t.ID] = action
	if action == color.UnknownPreserved {
		faults.Add(fault.Fault{Track: t.ID, Frame: -1, Kind: fault.KindUnknownColor,
			Recoverable: true, Detail: "unknown color tag preserved: " + tag})
	}

	jobs := make(chan frameJob, opts.QueueCap)
	results := make(chan FrameStat, opts.QueueCap)
	ctl := video.NewController(opts.TargetBPS, float64(fps.Num)/float64(fps.Den))
	var mu sync.Mutex
	var produced, done int64
	var wg sync.WaitGroup
	for w := 0; w < opts.Workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				img, err := decodeFrame(j.data)
				if err != nil {
					faults.Add(fault.Fault{Track: j.trackID, Frame: j.fr.Seq,
						Bytes: fault.ByteRange{End: int64(len(j.data))},
						Kind:  fault.KindStructure, Recoverable: true,
						Detail: "decode: " + err.Error()})
					continue
				}
				mu.Lock()
				q := ctl.Quality
				mu.Unlock()
				pr, err := video.EncodeProxy(img, q)
				if err != nil {
					faults.Add(fault.Fault{Track: j.trackID, Frame: j.fr.Seq,
						Kind: fault.KindStructure, Recoverable: true,
						Detail: "proxy: " + err.Error()})
					continue
				}
				mse := mseOf(pr)
				mu.Lock()
				produced += int64(len(pr.Data))
				done++
				ctl.Update(produced, done)
				mu.Unlock()
				select {
				case results <- FrameStat{Track: j.trackID, Seq: j.fr.Seq, PTS: j.fr.PTS,
					Bytes: int64(len(pr.Data)), MSE: mse, PSNR: psnrOf(mse)}:
				case <-ctx.Done():
					return
				}
			}
		}()
	}
	go func() {
		wg.Wait()
		close(results)
	}()

	// Producer: bounded read, hash verify, structure scan. A full job
	// queue applies backpressure here; cancellation stops the reads.
	producerErr := make(chan error, 1)
	go func() {
		defer close(jobs)
		for _, f := range frames {
			if badSeq[f.Seq] {
				continue
			}
			data, ok := verifyFrame(t, f, opts, faults)
			if !ok {
				continue
			}
			select {
			case jobs <- frameJob{trackID: t.ID, fr: f, data: data}:
			case <-ctx.Done():
				producerErr <- ctx.Err()
				return
			}
		}
	}()

	var stats []FrameStat
	for st := range results {
		stats = append(stats, st)
	}
	if err := <-producerErr; err != nil {
		return err
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	sort.Slice(stats, func(i, j int) bool { return stats[i].Seq < stats[j].Seq })
	res.Frames += len(stats)
	res.FrameStats = append(res.FrameStats, stats...)
	accumulate(clock, res, t.ID, frames[0], step, stats)
	return nil
}

// accumulate folds per-track stats into the run result: proxy bytes,
// timeline coverage and maximum alignment error.
func accumulate(clock timeline.Clock, res *Result, trackID string,
	base manifest.Frame, step int64, stats []FrameStat) {
	if len(stats) == 0 {
		return
	}
	var got, want []int64
	var minE, maxE int64
	var bytes int64
	for i, st := range stats {
		exp := base.PTS + (st.Seq-base.Seq)*step
		got = append(got, st.PTS)
		want = append(want, exp)
		if i == 0 || exp < minE {
			minE = exp
		}
		if i == 0 || exp > maxE {
			maxE = exp
		}
		bytes += st.Bytes
	}
	res.ProxyBytes += bytes
	res.DurationSec += float64(maxE-minE+step) * clock.Seconds(1)
	if a := alignMs(got, want, clock.Seconds(1)); a > res.AlignMaxMs {
		res.AlignMaxMs = a
	}
}
