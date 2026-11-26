package pipeline

import (
	"strconv"

	"avpipe/internal/media"
	"avpipe/internal/transport"
)

// Report is the full result of a pipeline run, consumed by the terminal UI.
type Report struct {
	Config           Config
	Segments         []SegmentReport
	Errors           []*media.Error
	Dedup            transport.DedupStats
	FEC              transport.FecStats
	Timeline         *Timeline
	CorruptPackets   int
	RecoveredPackets int
	GapFrames        int
	ResumeSegments   int
	PlaylistHash     [32]byte
	FrameHash        [32]byte
	Interrupted      bool
}

// NewReport initialises a report from the run configuration.
func NewReport(cfg Config) *Report {
	return &Report{Config: cfg}
}

func (r *Report) appendErrors(errs []*media.Error) {
	r.Errors = append(r.Errors, errs...)
}

// MaxAlignMS returns the worst representation alignment error: the maximum
// absolute difference between each segment duration and the ideal 1000 ms.
func (r *Report) MaxAlignMS() float64 {
	var max float64
	for _, s := range r.Segments {
		d := s.DurationMS - 1000.0
		if d < 0 {
			d = -d
		}
		if d > max {
			max = d
		}
	}
	return max
}

// MaxDriftMS returns the worst absolute AV drift observed.
func (r *Report) MaxDriftMS() float64 {
	var max float64
	for _, s := range r.Segments {
		if s.DriftMS < 0 {
			if -s.DriftMS > max {
				max = -s.DriftMS
			}
		} else if s.DriftMS > max {
			max = s.DriftMS
		}
	}
	return max
}

// MeanPSNR averages the luma PSNR across all encoded segments.
func (r *Report) MeanPSNR() float64 {
	if len(r.Segments) == 0 {
		return 0
	}
	var sum float64
	for _, s := range r.Segments {
		sum += s.PSNR
	}
	return sum / float64(len(r.Segments))
}

func parseInt64(s string) (int64, error)   { return strconv.ParseInt(s, 10, 64) }
func parseFloat(s string) (float64, error) { return strconv.ParseFloat(s, 64) }

func itoa(v int) string { return strconv.Itoa(v) }
