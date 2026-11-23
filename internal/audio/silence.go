// Package audio turns per-frame energy samples into silence ranges. The
// detector works on the shared nanosecond timeline and emits half-open
// intervals that partition cleanly: a boundary sample belongs to exactly one
// side.
package audio

import (
	"fmt"
	"math"
	"sort"

	"medpipe/internal/clock"
	"medpipe/internal/model"
)

// EnergySample is one block of audio energy with its duration on the shared
// timeline.
type EnergySample struct {
	TrackID  string
	FrameIdx int
	Seq      uint64
	StartNS  int64
	EndNS    int64
	Value    float64
}

// Options configures silence detection.
type Options struct {
	// WindowNS is the accumulation window. Zero means 20ms.
	WindowNS int64
	// Threshold is the mean-energy level at or below which a window counts as
	// silent. It is clamped to [0,1]. Zero is a valid threshold that matches
	// only digital silence.
	Threshold float64
	// MinSilenceNS drops detected runs shorter than this. Zero means one
	// window.
	MinSilenceNS int64
}

// DefaultOptions returns the usual 20ms / 0.05 pair.
func DefaultOptions() Options {
	return Options{WindowNS: 20_000_000, Threshold: 0.05, MinSilenceNS: 20_000_000}
}

// Range is a half-open silent interval [StartNS, EndNS).
type Range struct {
	TrackID string
	StartNS int64
	EndNS   int64
	// Windows counts the accumulation windows folded into the range.
	Windows int
}

// Collect gathers audio samples for every audio track from the timeline and
// the frame slices. Video tracks are ignored. Per-frame energy is spread
// evenly across the frame's interval, which keeps edge attribution exact.
func Collect(tl *clock.Timeline, frames map[string][]model.Frame) ([]EnergySample, error) {
	var out []EnergySample
	for i := range tl.Tracks {
		tt := &tl.Tracks[i]
		if tt.Track.Kind != model.KindAudio {
			continue
		}
		fs := frames[tt.Track.ID]
		if len(fs) != len(tt.Frames) {
			return nil, model.New(model.KindInvariant, "audio.Collect",
				fmt.Sprintf("track %s frame slices disagree (%d vs %d)", tt.Track.ID, len(fs), len(tt.Frames)))
		}
		if len(tt.Frames) == 0 {
			continue // an empty audio track yields no samples
		}
		for j := range tt.Frames {
			tf := &tt.Frames[j]
			fr := &fs[j]
			blocks := len(fr.Energy)
			span := tf.IntervalNS
			if span <= 0 {
				span = 1
			}
			if blocks == 0 {
				// A frame with no energy payload is one silent block.
				out = append(out, EnergySample{TrackID: tt.Track.ID, FrameIdx: j, Seq: fr.Seq, StartNS: tf.NS, EndNS: tf.NS + span})
				continue
			}
			for b := 0; b < blocks; b++ {
				s := tf.NS + span*int64(b)/int64(blocks)
				e := tf.NS + span*int64(b+1)/int64(blocks)
				out = append(out, EnergySample{
					TrackID: tt.Track.ID, FrameIdx: j, Seq: fr.Seq,
					StartNS: s, EndNS: e, Value: fr.Energy[b],
				})
			}
		}
	}
	sort.SliceStable(out, func(a, b int) bool {
		if out[a].TrackID != out[b].TrackID {
			return out[a].TrackID < out[b].TrackID
		}
		return out[a].StartNS < out[b].StartNS
	})
	return out, nil
}

// Detect folds samples into windows aligned to targetStart and returns silent
// ranges. A track shorter than one window still yields a verdict on its
// partial window. An empty track yields no ranges and no error. Samples that
// straddle a window boundary are split proportionally, so a boundary sample
// contributes to both windows but is never counted twice in full.
func Detect(samples []EnergySample, targetStart int64, opt Options) ([]Range, error) {
	if opt.WindowNS <= 0 {
		opt.WindowNS = 20_000_000
	}
	if opt.Threshold < 0 {
		opt.Threshold = 0
	}
	if opt.Threshold > 1 {
		opt.Threshold = 1
	}
	if opt.MinSilenceNS <= 0 {
		opt.MinSilenceNS = opt.WindowNS
	}
	byTrack := map[string][]EnergySample{}
	var order []string
	for _, s := range samples {
		if s.EndNS <= s.StartNS {
			return nil, model.New(model.KindInvariant, "audio.Detect",
				fmt.Sprintf("sample with non-positive duration on track %s", s.TrackID))
		}
		if _, ok := byTrack[s.TrackID]; !ok {
			order = append(order, s.TrackID)
		}
		byTrack[s.TrackID] = append(byTrack[s.TrackID], s)
	}
	sort.Strings(order)
	var out []Range
	for _, id := range order {
		out = append(out, detectTrack(id, byTrack[id], targetStart, opt)...)
	}
	return out, nil
}

type windowAcc struct {
	start   int64
	end     int64
	energy  float64
	samples int
}

func detectTrack(trackID string, samples []EnergySample, targetStart int64, opt Options) []Range {
	if len(samples) == 0 {
		return nil
	}
	sort.SliceStable(samples, func(a, b int) bool { return samples[a].StartNS < samples[b].StartNS })
	win := opt.WindowNS
	windows := map[int64]*windowAcc{}
	var order []int64
	for _, s := range samples {
		pos := s.StartNS
		for pos < s.EndNS {
			idx := floorDiv(pos-targetStart, win)
			wStart := targetStart + idx*win
			wEnd := wStart + win
			segEnd := s.EndNS
			if segEnd > wEnd {
				segEnd = wEnd
			}
			segLen := segEnd - pos
			if segLen <= 0 {
				break
			}
			frac := float64(segLen) / float64(s.EndNS-s.StartNS)
			a, ok := windows[wStart]
			if !ok {
				a = &windowAcc{start: wStart, end: wEnd}
				windows[wStart] = a
				order = append(order, wStart)
			}
			a.energy += s.Value * frac
			a.samples++
			pos = segEnd
		}
	}
	sort.Slice(order, func(i, j int) bool { return order[i] < order[j] })
	// Windows may extend past the media they cover; clamp output ranges to
	// the actual sample span so a partial window reports only real silence.
	mediaStart := samples[0].StartNS
	mediaEnd := samples[0].EndNS
	for _, s := range samples[1:] {
		if s.StartNS < mediaStart {
			mediaStart = s.StartNS
		}
		if s.EndNS > mediaEnd {
			mediaEnd = s.EndNS
		}
	}
	var ranges []Range
	var cur *Range
	flush := func() {
		if cur == nil {
			return
		}
		if cur.StartNS < mediaStart {
			cur.StartNS = mediaStart
		}
		if cur.EndNS > mediaEnd {
			cur.EndNS = mediaEnd
		}
		if cur.EndNS > cur.StartNS && cur.EndNS-cur.StartNS >= opt.MinSilenceNS {
			ranges = append(ranges, *cur)
		}
		cur = nil
	}
	for _, k := range order {
		a := windows[k]
		if a.samples == 0 {
			continue
		}
		silent := a.energy/float64(a.samples) <= opt.Threshold
		if !silent {
			flush()
			continue
		}
		if cur != nil && a.start == cur.EndNS {
			cur.EndNS = a.end
			cur.Windows++
			continue
		}
		// A gap between windows (or the first silent window) starts a new
		// range; ranges are never merged across non-silent or missing windows.
		flush()
		cur = &Range{TrackID: trackID, StartNS: a.start, EndNS: a.end, Windows: 1}
	}
	flush()
	return ranges
}

func floorDiv(a, b int64) int64 {
	q := a / b
	if a%b != 0 && (a < 0) != (b < 0) {
		q--
	}
	return q
}

// TotalSilenceNS sums the duration of ranges. Useful for reporting.
func TotalSilenceNS(rs []Range) int64 {
	var sum int64
	for _, r := range rs {
		sum += r.EndNS - r.StartNS
	}
	return sum
}

// EnergyMean is the mean of all sample values, or NaN for an empty slice.
func EnergyMean(samples []EnergySample) float64 {
	if len(samples) == 0 {
		return math.NaN()
	}
	var sum float64
	for _, s := range samples {
		sum += s.Value
	}
	return sum / float64(len(samples))
}
