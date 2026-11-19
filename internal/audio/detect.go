package audio

import (
	"fmt"

	"mediasync/internal/media"
)

// framePeak returns the largest absolute normalised sample in one frame.
func framePeak(samples []int16, frame, channels int) float64 {
	peak := 0.0
	base := frame * channels
	for c := 0; c < channels; c++ {
		v := float64(samples[base+c]) / 32768.0
		if v < 0 {
			v = -v
		}
		if v > peak {
			peak = v
		}
	}
	return peak
}

// runState tracks one candidate silence run while streaming frames.
type runState struct {
	inRun     bool
	start     int64
	end       int64 // exclusive, last known silent frame + 1
	peak      float64
	gapStart  int64
	gapFrames int64
}

// Detect scans PCM samples chunk by chunk. Silence runs are merged across
// chunk boundaries, while a loud stretch shorter than MinGapMS inside a run
// does not split it; a longer loud stretch does.
func Detect(samples []int16, format Format, opts Options) *Result {
	res := &Result{}
	if err := format.Validate(); err != nil {
		res.Issues = append(res.Issues, err.(*media.Issue).WithRecord(0))
		return res
	}
	opts = opts.normalize()
	frames := len(samples) / format.Channels
	if frames > MaxSamples {
		res.Issues = append(res.Issues, media.New(media.StageAudio, media.InputPCM, media.CodeLimitExceeded,
			"input has %d frames, limit is %d", frames, MaxSamples).WithRecord(MaxSamples))
		frames = MaxSamples
	}
	res.Frames = int64(frames)
	nsPerFrame := func(frame int64) int64 {
		return (frame*1_000_000_000 + int64(format.SampleRate)/2) / int64(format.SampleRate)
	}
	minSilenceFrames := int64(opts.MinSilenceMS) * int64(format.SampleRate) / 1000
	minGapFrames := int64(opts.MinGapMS) * int64(format.SampleRate) / 1000
	chunk := int64(opts.ChunkSamples)

	var st runState

	emit := func(start, end int64) {
		res.Runs = append(res.Runs, Run{
			StartSample: start,
			EndSample:   end,
			StartNS:     nsPerFrame(start),
			EndNS:       nsPerFrame(end),
			PeakDBFS:    dbfs(st.peak),
			Merged:      int((end-1)/chunk - start/chunk + 1),
		})
	}

	for f := int64(0); f < int64(frames); f++ {
		silent := dbfs(framePeak(samples, int(f), format.Channels)) <= opts.ThresholdDBFS
		if silent {
			res.SilentFrames++
			if !st.inRun {
				st = runState{inRun: true, start: f, end: f + 1}
			}
			st.gapFrames, st.gapStart = 0, 0
			st.end = f + 1
			if p := framePeak(samples, int(f), format.Channels); p > st.peak {
				st.peak = p
			}
			continue
		}
		if !st.inRun {
			continue
		}
		if st.gapFrames == 0 {
			st.gapStart = f
		}
		st.gapFrames++
		if st.gapFrames > minGapFrames {
			// The loud stretch is long enough to terminate the run.
			if st.end-st.start >= minSilenceFrames {
				emit(st.start, st.end)
			}
			res.SplitGaps++
			st = runState{}
		}
	}
	if st.inRun && st.end-st.start >= minSilenceFrames {
		emit(st.start, st.end)
	}
	res.Chunks = int((int64(frames) + chunk - 1) / chunk)
	for _, r := range res.Runs {
		if r.Merged > 1 {
			res.MergedChunks += r.Merged - 1
		}
	}
	detectIssues(res, opts)
	return res
}

func detectIssues(res *Result, opts Options) {
	prevEnd := int64(-1)
	for i, r := range res.Runs {
		if prevEnd >= 0 && r.StartSample < prevEnd {
			res.Issues = append(res.Issues, media.New(media.StageAudio, media.InputPCM, media.CodeOverlap,
				"silence run %d starts at frame %d before previous end %d", i, r.StartSample, prevEnd).WithRecord(i+1))
		}
		prevEnd = r.EndSample
		if r.DurationMS() < int64(opts.MinSilenceMS) {
			res.Issues = append(res.Issues, media.New(media.StageAudio, media.InputPCM, media.CodeSilenceEdge,
				"silence run %d (%dms) is shorter than %dms", i, r.DurationMS(), opts.MinSilenceMS).WithRecord(i+1))
		}
	}
}

// String renders a one-line description of a run.
func (r Run) String() string {
	return fmt.Sprintf("[%d.%03ds..%d.%03ds] %.1fdBFS", r.StartNS/1_000_000_000, (r.StartNS/1_000_000)%1000,
		r.EndNS/1_000_000_000, (r.EndNS/1_000_000)%1000, r.PeakDBFS)
}
