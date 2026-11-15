package verify

import (
	"encoding/json"
	"os"

	"m3u8ctl/internal/playlist"
)

// RenditionReport summarizes one ladder rung in the JSON report.
type RenditionReport struct {
	Rendition    int    `json:"rendition"`
	Name         string `json:"name"`
	BitrateBps   int    `json:"bitrate_bps"`
	Resolution   string `json:"resolution"`
	Segments     int    `json:"segments"`
	Gaps         int64  `json:"gaps"`
	GapIntervals int    `json:"gap_intervals"`
	MaxDriftMS   int64  `json:"max_drift_ms"`
	WindowStart  int64  `json:"window_start_seq"`
	WindowEnd    int64  `json:"window_end_seq"`
}

// Report is the stable, script-friendly verifier output.
type Report struct {
	Renditions     int               `json:"renditions"`
	Segments       int               `json:"segments"`
	Gaps           int64             `json:"gaps"`
	GapIntervals   int               `json:"gap_intervals"`
	DurationS      float64           `json:"duration_s"`
	TargetDuration int               `json:"target_duration"`
	DriftMaxMS     int64             `json:"drift_max_ms"`
	RoundTripOK    bool              `json:"round_trip_ok"`
	Duplicates     int               `json:"duplicate_conflicts"`
	Failures       []FailureDetail   `json:"failures"`
	RenditionStats []RenditionReport `json:"rendition_stats"`
}

// FailureDetail is one entry of the stable failures array.
type FailureDetail struct {
	Kind      string `json:"kind"`
	File      string `json:"file,omitempty"`
	Detail    string `json:"detail"`
	Rendition int    `json:"rendition,omitempty"`
	Seq       int64  `json:"seq,omitempty"`
}

// JSON serializes the report with stable field order (struct order) and
// trailing newline for shell pipelines.
func (report *Report) JSON() ([]byte, error) {
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// WriteJSON writes the report to path or stdout when path is empty or "-".
func (report *Report) WriteJSON(path string) error {
	data, err := report.JSON()
	if err != nil {
		return err
	}
	if path == "" || path == "-" {
		_, err := os.Stdout.Write(data)
		return err
	}
	return playlist.AtomicWriteFile(path, data)
}
