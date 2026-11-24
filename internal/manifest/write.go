// Package manifest writes deterministic processing and delivery manifests.
package manifest

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/clearliu/mediq/internal/core"
)

// Source describes the self-generated RTP/RTCP input and source range.
type Source struct {
	Case              string  `json:"case"`
	File              string  `json:"file"`
	SHA256            string  `json:"sha256"`
	SSRC              uint32  `json:"ssrc"`
	PayloadType       uint8   `json:"payload_type"`
	SampleRateHz      int     `json:"sample_rate_hz"`
	Channels          int     `json:"channels"`
	FramesPerPacket   int     `json:"frames_per_packet"`
	RTPSeqFirst       uint16  `json:"rtp_seq_first"`
	RTPSeqLast        uint16  `json:"rtp_seq_last"`
	ExtendedSeqFirst  int64   `json:"extended_seq_first"`
	ExtendedSeqLast   int64   `json:"extended_seq_last"`
	RTPTimestampFirst uint32  `json:"rtp_timestamp_first"`
	RTPTimestampLast  uint32  `json:"rtp_timestamp_last"`
	StartTimeMS       float64 `json:"start_time_ms"`
	EndTimeMS         float64 `json:"end_time_ms"`
}

// Params records all processing parameters bound to the checkpoint.
type Params struct {
	OutputRateHz        int     `json:"output_rate_hz"`
	OutputChannels      int     `json:"output_channels"`
	OutputBitsPerSample int     `json:"output_bits_per_sample"`
	TargetBPS           int64   `json:"target_bps"`
	Resampler           string  `json:"resampler"`
	LoudnessTargetLUFS  float64 `json:"loudness_target_lufs"`
	TruePeakCeilingDBTP float64 `json:"true_peak_ceiling_dbtp"`
	ReorderWindow       int     `json:"reorder_window"`
	TimelineIntervalMS  int     `json:"timeline_interval_ms"`
	RetentionIntervals  int     `json:"retention_intervals"`
	MaxDatagramBytes    int     `json:"max_datagram_bytes"`
}

// Counts contains parsing, loss and replay quality counters.
type Counts struct {
	RTPFrames        int      `json:"rtp_frames"`
	RTCPFrames       int      `json:"rtcp_frames"`
	ParseErrors      int      `json:"parse_errors"`
	DuplicatePackets int      `json:"duplicate_packets"`
	OutOfOrder       int      `json:"out_of_order_packets"`
	GapPackets       int      `json:"gap_packets"`
	LatePackets      int      `json:"late_packets"`
	LateReplays      int      `json:"late_replays"`
	LateExpired      int      `json:"late_expired"`
	MissingSamples   int64    `json:"missing_samples"`
	MissingSeqRanges []string `json:"missing_seq_ranges,omitempty"`
}

// Quality contains bitrate, loudness and timeline verification values.
type Quality struct {
	TargetBPS       int64   `json:"target_bps"`
	ActualBPS       float64 `json:"actual_bps"`
	DeviationPct    float64 `json:"deviation_pct"`
	DurationMS      float64 `json:"duration_ms"`
	ExpectedMS      float64 `json:"expected_duration_ms"`
	ManifestErrorMS float64 `json:"manifest_error_ms"`
	IntegratedLUFS  float64 `json:"integrated_lufs"`
	TruePeakDBTP    float64 `json:"true_peak_dbtp"`
}

// Product identifies the MP4 artifact and hash.
type Product struct {
	File     string `json:"file"`
	SHA256   string `json:"sha256"`
	Bytes    int64  `json:"bytes"`
	PCMBytes int    `json:"pcm_bytes"`
}

// Manifest is the complete delivery record.
type Manifest struct {
	SchemaVersion          int     `json:"schema_version"`
	Status                 string  `json:"status"`
	ExitCode               int     `json:"exit_code"`
	Strict                 bool    `json:"strict"`
	FirstParseErrorOffset  int64   `json:"first_parse_error_offset"`
	FirstParseError        string  `json:"first_parse_error,omitempty"`
	InputExceptionCount    int     `json:"input_exception_count"`
	CommittedOutputSamples int64   `json:"committed_output_samples"`
	CheckpointSHA256       string  `json:"checkpoint_sha256,omitempty"`
	Source                 Source  `json:"source"`
	Params                 Params  `json:"params"`
	Counts                 Counts  `json:"counts"`
	Quality                Quality `json:"quality"`
	Product                Product `json:"product"`
}

// Write atomically writes manifest.json.
func Write(path string, m *Manifest) error {
	m.SchemaVersion = 1
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return core.New(core.KindConfig, "encode-manifest", err)
	}
	data = append(data, '\n')
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return core.New(core.KindIO, "create-manifest-dir", err)
	}
	tmp, err := os.CreateTemp(dir, ".manifest-*.tmp")
	if err != nil {
		return core.New(core.KindIO, "create-manifest-temp", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return core.New(core.KindIO, "write-manifest", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return core.New(core.KindIO, "sync-manifest", err)
	}
	if err := tmp.Close(); err != nil {
		return core.New(core.KindIO, "close-manifest", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return core.New(core.KindIO, "rename-manifest", err)
	}
	return nil
}
