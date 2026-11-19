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

// windowPeak returns the peak of frames [start, end).
func windowPeak(samples []int16, start, end, channels int) float64 {
	peak := 0.0
	for f := start; f < end; f++ {
		if p := framePeak(samples, f, channels); p > peak {
			peak = p
		}
	}
	return peak
}

// runState tracks one candidate silence run in frames.
type runState struct {
	inRun     bool
	start     int64
	end       int64 // exclusive, end of the last silent window
	peak      float64
	gapFrames int64 // loud frames since the last silent window
}

// Detect scans PCM samples in chunks, classifying 10 ms windows on a global
// grid so a wave zero-crossing is never mistaken for silence. Silence runs are
// merged across chunk boundaries; a loud stretch shorter than MinGapMS inside
// a run does not split it, while a longer stretch does.
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
	rate := int64(format.SampleRate)
	nsPerFrame := func(frame int64) int64 {
		return (frame*1_000_000_000 + rate/2) / rate
	}
	win := int(rate / 100)
	if win < 1 {
		win = 1
	}
	minSilence := int64(opts.MinSilenceMS) * rate / 1000
	minGap := int64(opts.MinGapMS) * rate / 1000
	chunk := opts.ChunkSamples
	if chunk <= 0 {
		chunk = 1024
	}

	var st runState
	closeRun := func() {
		if st.inRun && st.end-st.start >= minSilence {
			first := st.start / int64(chunk)
			last := (st.end - 1) / int64(chunk)
			res.Runs = append(res.Runs, Run{
				StartSample: st.start,
				EndSample:   st.end,
				StartNS:     nsPerFrame(st.start),
				EndNS:       nsPerFrame(st.end),
				PeakDBFS:    dbfs(st.peak),
				Merged:      int(last - first + 1),
			})
		}
		st = runState{}
	}

	res.Chunks = (frames + chunk - 1) / chunk
	for wStart := 0; wStart < frames; wStart += win {
		wEnd := wStart + win
		if wEnd > frames {
			wEnd = frames
		}
		peak := windowPeak(samples, wStart, wEnd, format.Channels)
		if dbfs(peak) <= opts.ThresholdDBFS {
			res.SilentFrames += int64(wEnd - wStart)
			if !st.inRun {
				st.inRun, st.start, st.peak = true, int64(wStart), 0
			}
			st.gapFrames = 0
			st.end = int64(wEnd)
			if peak > st.peak {
				st.peak = peak
			}
			continue
		}
		if !st.inRun {
			continue
		}
		st.gapFrames += int64(wEnd - wStart)
		if st.gapFrames > minGap {
			closeRun()
			res.SplitGaps++
		}
	}
	closeRun()
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
