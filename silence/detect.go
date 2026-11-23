// Package silence detects silent audio windows by RMS energy. Windows whose
// data is unavailable (damaged, missing, or not covered by any sample) are
// reported as unknown and are never judged silent.
package silence

import (
	"encoding/binary"
	"io"
	"math"

	"bcast/media"
)

// Status classifies a window or the whole report.
type Status string

const (
	StatusOK            Status = "ok"
	StatusNoAudioTrack  Status = "no_audio_track"
	StatusTSRegression  Status = "timestamp_regression"
	StatusCrossesDamage Status = "window_crosses_damaged_range"
	StatusGap           Status = "window_not_covered"
)

// Window is one analysis window.
type Window struct {
	Index  int
	Start  media.Time // audio timescale
	End    media.Time
	RMS    float64
	Silent bool
	Status Status
}

// Report is the outcome of one detection run.
type Report struct {
	Windows        []Window
	SilentWindows  int
	UnknownWindows int
	Status         Status // overall status
}

// Detect measures RMS energy per window of windowSize audio-timescale
// units. A window is silent only when every audio sample overlapping it is
// present, readable, and its RMS is below threshold. Any damaged range or
// uncovered span makes the window unknown (never silent), and the unknown
// count is reported so callers can tell "quiet" from "cannot tell".
func Detect(f *media.File, src io.ReaderAt, windowSize int64, threshold float64) *Report {
	at, ok := f.TrackByKind(media.TrackAudio)
	if !ok {
		return &Report{Status: StatusNoAudioTrack}
	}
	if windowSize <= 0 {
		return &Report{Status: StatusNoAudioTrack}
	}
	rep := &Report{Status: StatusOK}
	samples := f.SamplesOf(at.ID)
	if len(samples) == 0 {
		return rep
	}
	for i := 1; i < len(samples); i++ {
		if samples[i].DTS.Value < samples[i-1].DTS.Value {
			rep.Status = StatusTSRegression
			return rep
		}
	}
	// Analyse the full declared track duration so windows over missing tail
	// data are still emitted (and reported as unknown, not silent).
	total := at.Duration
	if total <= 0 {
		total = samples[len(samples)-1].End().Value
	}
	lastEnd := int64(0)
	for _, s := range samples {
		if e := s.End().Value; e > lastEnd {
			lastEnd = e
		}
	}
	// A window must be fully covered by present audio; a partially covered
	// window is unknown rather than a silent reading of the covered part.
	idx := 0
	for w := int64(0); w*windowSize < total; w++ {
		win := Window{
			Index:  int(w),
			Start:  media.Time{Value: w * windowSize, Timescale: at.Timescale},
			End:    media.Time{Value: (w + 1) * windowSize, Timescale: at.Timescale},
			Status: StatusOK,
		}
		var sumSq float64
		var n int
		covered := int64(0) // audio units covered inside [Start, End)
		reason := StatusOK
		for idx < len(samples) && samples[idx].DTS.Value < win.End.Value {
			s := samples[idx]
			lo, hi := maxInt64(s.DTS.Value, win.Start.Value), minInt64(s.End().Value, win.End.Value)
			if hi > lo {
				covered += hi - lo
				if !sampleAvailable(f, s) {
					reason = StatusCrossesDamage
				} else if rms, cnt, err := sampleRMS(src, s); err != nil {
					reason = StatusCrossesDamage
				} else {
					sumSq += rms * rms * float64(cnt)
					n += cnt
				}
			}
			if s.End().Value <= win.End.Value {
				idx++
			} else {
				break
			}
		}
		switch {
		case covered < windowSize:
			if reason == StatusOK {
				// Nothing unreadable was hit, but the window is incomplete.
				// Past the last present sample this is the truncated tail,
				// which is a damaged range rather than a clean hole.
				if win.End.Value > lastEnd && len(f.Damaged) > 0 {
					reason = StatusCrossesDamage
				} else {
					reason = StatusGap
				}
			}
			win.Status = reason
			rep.UnknownWindows++
		case reason != StatusOK:
			win.Status = reason
			rep.UnknownWindows++
		case n == 0:
			win.Status = StatusCrossesDamage // covered but no PCM values
			rep.UnknownWindows++
		default:
			win.RMS = math.Sqrt(sumSq / float64(n))
			win.Silent = win.RMS < threshold
			if win.Silent {
				rep.SilentWindows++
			}
		}
		rep.Windows = append(rep.Windows, win)
	}
	return rep
}

// sampleAvailable reports whether a sample's payload is inside the file and
// clear of every damaged range.
func sampleAvailable(f *media.File, s media.Sample) bool {
	start, end := s.Offset, s.Offset+int64(s.Size)
	if end > fileEnd(f) {
		return false
	}
	for _, d := range f.Damaged {
		if start < d.End && end > d.Start {
			return false // any overlap means the sample crosses damage
		}
	}
	return true
}

func fileEnd(f *media.File) int64 {
	var end int64
	for _, s := range f.Samples {
		if e := s.Offset + int64(s.Size); e > end {
			end = e
		}
	}
	for _, d := range f.Damaged {
		if d.End > end {
			end = d.End
		}
	}
	return end
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

// sampleRMS reads one sample's payload as little-endian int16 PCM and
// returns its RMS and value count.
func sampleRMS(src io.ReaderAt, s media.Sample) (float64, int, error) {
	buf := make([]byte, s.Size)
	if _, err := src.ReadAt(buf, s.Offset); err != nil {
		return 0, 0, err
	}
	n := len(buf) / 2
	if n == 0 {
		return 0, 0, nil
	}
	var sumSq float64
	for i := 0; i < n; i++ {
		v := float64(int16(binary.LittleEndian.Uint16(buf[i*2:])))
		sumSq += v * v
	}
	return math.Sqrt(sumSq / float64(n)), n, nil
}
