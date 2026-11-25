// Package silence computes PCM RMS windows and detects sustained silence.
package silence

import (
	"math"
)

const (
	SampleRate            = 48000
	Channels              = 2
	BytesPerSample        = 2
	FrameBytes            = Channels * BytesPerSample // 4 bytes per stereo frame
	WindowMS              = 20
	WindowFrames          = SampleRate * WindowMS / 1000 // 960 frames
	WindowBytes           = WindowFrames * FrameBytes    // 3840 bytes
	WindowNS        int64 = WindowMS * 1_000_000
	ThresholdDBFS         = -48.0
	MinSilenceNS    int64 = 500_000_000
	MinQuietWindows       = int(MinSilenceNS / WindowNS) // 25
)

// Gap marks a time interval for which PCM samples are unavailable.
type Gap struct {
	StartNS int64 `json:"start_ns"`
	EndNS   int64 `json:"end_ns"`
}

// Window is one 20 ms RMS analysis window.
type Window struct {
	Index   int     `json:"index"`
	StartNS int64   `json:"start_ns"`
	EndNS   int64   `json:"end_ns"`
	RMS     float64 `json:"rms"`
	DBFS    float64 `json:"dbfs"`
	Silent  bool    `json:"silent"`
	Unknown bool    `json:"unknown"`
}

// Span is a contiguous silence or unknown interval.
type Span struct {
	StartNS int64 `json:"start_ns"`
	EndNS   int64 `json:"end_ns"`
}

// Result contains windows and consolidated intervals.
type Result struct {
	Windows        []Window `json:"windows"`
	Silence        []Span   `json:"silence"`
	Unknown        []Span   `json:"unknown"`
	SilenceNS      int64    `json:"silence_ns"`
	UnknownNS      int64    `json:"unknown_ns"`
	TotalWindows   int      `json:"total_windows"`
	SilentWindows  int      `json:"silent_windows"`
	UnknownWindows int      `json:"unknown_windows"`
}

// DetectS16LE analyzes 48 kHz stereo signed little-endian PCM. Missing time
// ranges make intersecting windows unknown rather than silent.
func DetectS16LE(pcm []byte, gaps []Gap) Result {
	n := len(pcm) / WindowBytes
	res := Result{Windows: make([]Window, 0, n)}
	for i := 0; i < n; i++ {
		start := int64(i) * WindowNS
		end := start + WindowNS
		w := Window{Index: i, StartNS: start, EndNS: end}
		w.RMS, w.DBFS = windowLevel(pcm[i*WindowBytes : (i+1)*WindowBytes])
		w.Unknown = intersectsGap(start, end, gaps)
		w.Silent = !w.Unknown && w.DBFS < ThresholdDBFS
		if w.Unknown {
			res.UnknownWindows++
		}
		if w.Silent {
			res.SilentWindows++
		}
		res.Windows = append(res.Windows, w)
	}
	res.TotalWindows = n
	res.Silence = collectRuns(res.Windows, func(w Window) bool { return w.Silent }, MinQuietWindows)
	res.Unknown = collectRuns(res.Windows, func(w Window) bool { return w.Unknown }, 1)
	for _, s := range res.Silence {
		res.SilenceNS += s.EndNS - s.StartNS
	}
	for _, s := range res.Unknown {
		res.UnknownNS += s.EndNS - s.StartNS
	}
	return res
}

func windowLevel(b []byte) (float64, float64) {
	var sumSquares float64
	samples := len(b) / 2
	for i := 0; i+1 < len(b); i += 2 {
		v := int16(uint16(b[i]) | uint16(b[i+1])<<8)
		f := float64(v)
		sumSquares += f * f
	}
	if samples == 0 {
		return 0, -math.Inf(1)
	}
	rms := math.Sqrt(sumSquares / float64(samples))
	if rms == 0 {
		return 0, -120
	}
	db := 20 * math.Log10(rms/32768.0)
	if db < -120 {
		db = -120
	}
	return rms, db
}

func intersectsGap(start, end int64, gaps []Gap) bool {
	for _, g := range gaps {
		if g.StartNS < end && start < g.EndNS {
			return true
		}
	}
	return false
}

func collectRuns(ws []Window, match func(Window) bool, minWindows int) []Span {
	var out []Span
	runStart := -1
	flush := func(endIndex int) {
		if runStart < 0 {
			return
		}
		count := endIndex - runStart
		if count >= minWindows {
			out = append(out, Span{
				StartNS: ws[runStart].StartNS,
				EndNS:   ws[endIndex-1].EndNS,
			})
		}
		runStart = -1
	}
	for i, w := range ws {
		if match(w) {
			if runStart < 0 {
				runStart = i
			}
		} else {
			flush(i)
		}
	}
	flush(len(ws))
	return out
}

// AppendS16 appends one stereo S16LE frame with the same value on both channels.
func AppendS16(dst []byte, sample int16) []byte {
	var b [4]byte
	u := uint16(sample)
	b[0] = byte(u)
	b[1] = byte(u >> 8)
	b[2] = b[0]
	b[3] = b[1]
	return append(dst, b[:]...)
}

// TonePCM builds deterministic PCM with a fixed-amplitude square/sine-ish tone.
// amplitude=0 produces digital silence.
func TonePCM(windows int, amplitude int16) []byte {
	pcm := make([]byte, 0, windows*WindowBytes)
	for f := 0; f < windows*WindowFrames; f++ {
		v := amplitude
		if amplitude > 0 && f%48 < 24 { // 1 kHz-ish square at 48k/48? period48
			v = amplitude
		} else if amplitude > 0 {
			v = -amplitude
		}
		pcm = AppendS16(pcm, v)
	}
	return pcm
}
