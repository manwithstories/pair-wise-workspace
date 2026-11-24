package report

import (
	"fmt"
	"sort"
	"strings"

	"mediarig/internal/audio"
	"mediarig/internal/media"
	"mediarig/internal/mp4"
)

// Stats is the stable single-line machine output. JSON tags support archived
// sidecars; KV returns the exact terminal key=value contract.
type Stats struct {
	TargetKbps       float64 `json:"target_kbps"`
	ActualKbps       float64 `json:"actual_kbps"`
	DeviationPct     float64 `json:"deviation_pct"`
	LostPackets      int     `json:"lost_packets"`
	ReorderedPackets int     `json:"reordered_packets"`
	DuplicatePackets int     `json:"duplicate_packets"`
	LUFSBefore       float64 `json:"lufs_before"`
	LUFSAfter        float64 `json:"lufs_after"`
	TruePeakDBTP     float64 `json:"true_peak_dbtp"`
	ManifestSkewUS   float64 `json:"manifest_skew_us"`
	Commit           string  `json:"commit"`
	Committed        bool    `json:"committed"`
	ClippedSamples   int     `json:"clipped_samples"`
	SilenceRegions   int     `json:"silence_regions"`
	ManifestSHA256   string  `json:"manifest_sha256,omitempty"`
	Recovered        bool    `json:"recovered,omitempty"`
}

// KV renders one stable sorted-ish contract line. Required acceptance keys
// retain their exact names and order.
func (s Stats) KV() string {
	keys := []struct {
		k string
		v string
	}{
		{"target_kbps", fmt.Sprintf("%.3f", s.TargetKbps)},
		{"actual_kbps", fmt.Sprintf("%.3f", s.ActualKbps)},
		{"deviation_pct", fmt.Sprintf("%.4f", s.DeviationPct)},
		{"lost_packets", fmt.Sprintf("%d", s.LostPackets)},
		{"reordered_packets", fmt.Sprintf("%d", s.ReorderedPackets)},
		{"lufs_before", fmt.Sprintf("%.3f", s.LUFSBefore)},
		{"lufs_after", fmt.Sprintf("%.3f", s.LUFSAfter)},
		{"true_peak_dbtp", fmt.Sprintf("%.3f", s.TruePeakDBTP)},
		{"manifest_skew_us", fmt.Sprintf("%.3f", s.ManifestSkewUS)},
		{"commit", s.Commit},
	}
	parts := make([]string, 0, len(keys)+6)
	for _, kv := range keys {
		parts = append(parts, kv.k+"="+kv.v)
	}
	parts = append(parts,
		fmt.Sprintf("duplicate_packets=%d", s.DuplicatePackets),
		fmt.Sprintf("clipped_samples=%d", s.ClippedSamples),
		fmt.Sprintf("silence_regions=%d", s.SilenceRegions),
		fmt.Sprintf("committed=%t", s.Committed),
	)
	if s.ManifestSHA256 != "" {
		parts = append(parts, "manifest_sha256="+s.ManifestSHA256)
	}
	if s.Recovered {
		parts = append(parts, "recovered=true")
	}
	return strings.Join(parts, " ")
}

// TrackTimeline is one SSRC-aligned timeline entry.
type TrackTimeline struct {
	SSRC           uint32 `json:"ssrc"`
	Kind           string `json:"kind"`
	Payload        string `json:"payload"`
	ClockRate      int    `json:"clock_rate"`
	StartMediaTime string `json:"start_media_time_seconds"`
	Duration       string `json:"duration_seconds"`
	Duration48k    int64  `json:"duration_48k_samples"`
	Packets        int    `json:"packets"`
	MissingPackets int    `json:"missing_packets"`
}

// Manifest is the program timeline and adjudication archive.
type Manifest struct {
	SchemaVersion   int                  `json:"schema_version"`
	Commit          string               `json:"commit"`
	InputSHA256     string               `json:"input_sha256"`
	ParamsSHA256    string               `json:"params_sha256"`
	DurationSamples int64                `json:"duration_48k_samples"`
	DurationSeconds string               `json:"duration_seconds_rational"`
	MaxSkewSamples  string               `json:"max_skew_samples_rational"`
	MaxSkewUS       float64              `json:"max_skew_us"`
	Tracks          []TrackTimeline      `json:"tracks"`
	Color           mp4.ColorDecision    `json:"color"`
	Loudness        audio.LoudnessReport `json:"loudness"`
	MP4Validation   *mp4.Validation      `json:"mp4_validation,omitempty"`
	TargetKbps      float64              `json:"target_kbps"`
	ActualKbps      float64              `json:"actual_kbps"`
}

// MaxAllowedSkewUS is one 48 kHz audio sample: 1e6/48000 = 20.833... us.
const MaxAllowedSkewUS = 1000000.0 / 48000.0

// SkewUS converts an exact rational sample-error count at 48 kHz to microseconds.
func SkewUS(skewSamples media.Rational) float64 {
	return skewSamples.MulRat(media.MustRat(1_000_000, 48000)).Float64()
}

// SortTracks gives deterministic manifest output.
func SortTracks(t []TrackTimeline) {
	sort.Slice(t, func(i, j int) bool { return t[i].SSRC < t[j].SSRC })
}

// ErrorLine is the stable boundary/failure report.
type ErrorLine struct {
	ErrorStage     string  `json:"error_stage"`
	PacketSequence *uint16 `json:"packet_sequence,omitempty"`
	RecordIndex    *int    `json:"record_index,omitempty"`
	Committed      bool    `json:"committed"`
	Commit         string  `json:"commit"`
	ExitCode       int     `json:"exit_code"`
	Message        string  `json:"message"`
}

func (e ErrorLine) KV() string {
	s := fmt.Sprintf("exit_code=%d error_stage=%s stage=%s committed=%t commit=%s",
		e.ExitCode, e.ErrorStage, e.ErrorStage, e.Committed, e.Commit)
	if e.PacketSequence != nil {
		s += fmt.Sprintf(" packet_sequence=%d rtp_seq=%d", *e.PacketSequence, *e.PacketSequence)
	}
	if e.RecordIndex != nil {
		s += fmt.Sprintf(" record_index=%d", *e.RecordIndex)
	}
	s += " message=" + quoteKV(e.Message)
	return s
}

func quoteKV(s string) string {
	s = strings.ReplaceAll(s, " ", "_")
	s = strings.ReplaceAll(s, ":", "")
	return s
}
