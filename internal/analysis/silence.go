package analysis

import (
	"math"

	"github.com/asticode/go-astiav"
)

// SilenceConfig tunes the adaptive, per-channel silence detector.
type SilenceConfig struct {
	// MinSilentFrames is the run length required to report a silence interval.
	MinSilentFrames int
	// FloorQuantile picks the RMS level treated as the local noise floor,
	// expressed as a fraction (0..1) of frames sorted by loudness.
	FloorQuantile float64
	// AbsoluteFloor is the linear amplitude below which a frame is silent.
	// It also serves as the minimum margin above an elevated noise floor.
	AbsoluteFloor float32
}

// DefaultSilenceConfig returns conservative defaults. The absolute floor is
// chosen above genuine digital silence (exactly 0.0) but well below the
// quietest real audio frame, including an AAC encoder priming frame.
func DefaultSilenceConfig() SilenceConfig {
	return SilenceConfig{
		MinSilentFrames: 2,
		FloorQuantile:   0.2,
		AbsoluteFloor:   0.01,
	}
}

// ChannelSilence is the per-channel silence result.
type ChannelSilence struct {
	Channel       int     `json:"channel"`
	TotalSamples  int     `json:"total_samples"`
	SilentSamples int     `json:"silent_samples"`
	MaxRMS        float32 `json:"max_rms"`
	// Intervals are silence ranges in 90 kHz ticks.
	Intervals []Interval `json:"intervals"`
}

// Interval is a half-open [Start90k, End90k) range on the shared clock.
type Interval struct {
	Start90k int64 `json:"start_90k"`
	End90k   int64 `json:"end_90k"`
}

// SilenceReport aggregates per-channel silence for one segment.
type SilenceReport struct {
	Path             string           `json:"path"`
	SampleRate       int              `json:"sample_rate"`
	Channels         int              `json:"channels"`
	ChannelsSilence  []ChannelSilence `json:"channels_silence"`
	// AnySilent is true when at least one channel has a qualifying interval.
	AnySilent bool `json:"any_silent"`
}

// frameLevel is one decoded audio frame's per-channel RMS plus timing.
type frameLevel struct {
	rms      []float32
	start90k int64
	end90k   int64
}

// DetectSilence decodes the audio track and applies a local, per-channel noise
// gate. Each channel is judged independently, satisfying the per-channel
// statistics requirement.
func DetectSilence(path string, cfg SilenceConfig) (*SilenceReport, error) {
	_, streams, closer, err := openSegment(path)
	if err != nil {
		return nil, err
	}
	defer closer()

	ds, ok := streams[astiav.MediaTypeAudio]
	if !ok {
		return &SilenceReport{Path: path}, nil
	}
	timeBase := ds.stream.TimeBase()
	channels := ds.codecCtx.ChannelLayout().Channels()
	sampleRate := ds.codecCtx.SampleRate()

	var levels []frameLevel
	err = decodeFrames(streams, astiav.MediaTypeAudio, func(f *astiav.Frame) error {
		rms, err := frameRMSPerChannel(f, channels)
		if err != nil {
			return err
		}
		start90k := to90k(f.Pts(), timeBase)
		end90k := to90k(f.Pts()+int64(f.NbSamples()), timeBase)
		levels = append(levels, frameLevel{rms: rms, start90k: start90k, end90k: end90k})
		return nil
	})
	if err != nil {
		return nil, err
	}

	report := &SilenceReport{
		Path: path, SampleRate: sampleRate, Channels: channels,
	}
	for ch := 0; ch < channels; ch++ {
		cs := classifyChannel(ch, levels, cfg)
		report.ChannelsSilence = append(report.ChannelsSilence, cs)
		if len(cs.Intervals) > 0 {
			report.AnySilent = true
		}
	}
	return report, nil
}

// frameRMSPerChannel extracts planar/interleaved float samples and computes
// the RMS amplitude of each channel within the frame.
func frameRMSPerChannel(f *astiav.Frame, channels int) ([]float32, error) {
	buf, err := f.Data().Bytes(1)
	if err != nil {
		return nil, err
	}
	n := f.NbSamples()
	rms := make([]float32, channels)
	planar := isPlanar(f.SampleFormat())
	// FrameData.Bytes returns a tightly packed copy: each planar channel spans
	// exactly nbSamples*bytesPerSample (NOT linesize, which describes the
	// aligned in-frame stride). Using linesize here overshoots into the next
	// plane and makes every channel after the first read as silence.
	planeBytes := n * 4

	for ch := 0; ch < channels; ch++ {
		var sumSq float64
		count := 0
		for i := 0; i < n; i++ {
			var idx int
			if planar {
				idx = ch*planeBytes + i*4
			} else {
				idx = (i*channels + ch) * 4
			}
			if idx+4 > len(buf) {
				break
			}
			v := math.Float32frombits(le32(buf[idx : idx+4]))
			sumSq += float64(v) * float64(v)
			count++
		}
		if count > 0 {
			rms[ch] = float32(math.Sqrt(sumSq / float64(count)))
		}
	}
	return rms, nil
}

// classifyChannel estimates a per-channel noise floor from the quietest
// fraction of frames, then gates each frame against the larger of an absolute
// silence floor and a relative multiple of the estimated floor. This keeps a
// steady tone (whose local minimum equals its own level) from being treated as
// silence while genuine near-zero regions are detected.
func classifyChannel(ch int, levels []frameLevel, cfg SilenceConfig) ChannelSilence {
	cs := ChannelSilence{Channel: ch}
	if len(levels) == 0 {
		return cs
	}

	series := make([]float32, 0, len(levels))
	for _, lv := range levels {
		if ch < len(lv.rms) {
			series = append(series, lv.rms[ch])
		}
	}
	if len(series) == 0 {
		return cs
	}

	// Threshold model:
	floor := quantile(series, cfg.FloorQuantile)
	//   - A high floor means the whole segment is active (e.g. a steady tone);
	//     gating against that floor would mark everything silent. In that case
	//     only the absolute floor is meaningful.
	//   - A low floor indicates real near-silence is present, so a small margin
	//     above the measured floor separates silence from low-level noise.
	threshold := cfg.AbsoluteFloor
	if floor <= cfg.AbsoluteFloor {
		if margin := floor + cfg.AbsoluteFloor; margin > threshold {
			threshold = margin
		}
	}

	silentFlags := make([]bool, len(levels))
	for i, lv := range levels {
		if ch >= len(lv.rms) {
			continue
		}
		rms := lv.rms[ch]
		if rms > cs.MaxRMS {
			cs.MaxRMS = rms
		}
		cs.TotalSamples++
		if rms <= threshold {
			silentFlags[i] = true
		}
	}

	// Group consecutive silent frames into intervals, honouring the minimum run.
	runStart := -1
	flush := func(end int) {
		if runStart >= 0 && end-runStart >= cfg.MinSilentFrames {
			cs.Intervals = append(cs.Intervals, Interval{
				Start90k: levels[runStart].start90k,
				End90k:   levels[end-1].end90k,
			})
			for k := runStart; k < end; k++ {
				cs.SilentSamples++
			}
		}
		runStart = -1
	}
	for i, silent := range silentFlags {
		if silent {
			if runStart < 0 {
				runStart = i
			}
			continue
		}
		flush(i)
	}
	flush(len(levels))
	return cs
}

// quantile returns the q-quantile (0..1) of a copy of the series without
// mutating the caller's ordering.
func quantile(series []float32, q float64) float32 {
	cp := make([]float32, len(series))
	copy(cp, series)
	// Insertion sort is fine for the short per-segment frame counts.
	for i := 1; i < len(cp); i++ {
		v := cp[i]
		j := i - 1
		for j >= 0 && cp[j] > v {
			cp[j+1] = cp[j]
			j--
		}
		cp[j+1] = v
	}
	idx := int(math.Round(q * float64(len(cp)-1)))
	if idx < 0 {
		idx = 0
	}
	if idx >= len(cp) {
		idx = len(cp) - 1
	}
	return cp[idx]
}

func isPlanar(fmt astiav.SampleFormat) bool {
	switch fmt {
	case astiav.SampleFormatFltp, astiav.SampleFormatS16P,
		astiav.SampleFormatS32P, astiav.SampleFormatDblp:
		return true
	default:
		return false
	}
}

func to90k(ts int64, tb astiav.Rational) int64 {
	if tb.Den() == 0 {
		return 0
	}
	sec := float64(ts) * float64(tb.Num()) / float64(tb.Den())
	return int64(math.Round(sec * 90000))
}

func le32(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}
