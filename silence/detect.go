// Package silence detects silent audio windows by RMS energy. Windows
// whose data is unavailable (damaged or missing) are reported as unknown
// and never judged silent.
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
	Windows       []Window
	SilentWindows int
	Status        Status // overall status
}

// Detect measures RMS energy per window of windowSize audio-timescale
// units. A window is silent when its RMS falls below threshold. Damaged or
// missing data yields StatusCrossesDamage windows, never silence.
func Detect(f *media.File, src io.ReaderAt, windowSize int64, threshold float64) *Report {
	at, ok := f.TrackByKind(media.TrackAudio)
	if !ok {
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
	total := samples[len(samples)-1].End().Value
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
		covered := true
		for idx < len(samples) && samples[idx].DTS.Value < win.End.Value {
			s := samples[idx]
			if s.End().Value > win.Start.Value {
				if s.Offset+int64(s.Size) > fileEnd(f) || inDamaged(f, s.Offset) {
					covered = false
				} else {
					rms, cnt, err := sampleRMS(src, s)
					if err != nil {
						covered = false
					} else {
						sumSq += rms * rms * float64(cnt)
						n += cnt
					}
				}
			}
			if s.End().Value <= win.End.Value {
				idx++
			} else {
				break
			}
		}
		switch {
		case !covered || n == 0:
			win.Status = StatusCrossesDamage // unknown: never silent
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

func inDamaged(f *media.File, off int64) bool {
	for _, d := range f.Damaged {
		if off >= d.Start && off < d.End {
			return true
		}
	}
	return false
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
