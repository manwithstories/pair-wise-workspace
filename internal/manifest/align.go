// Package manifest cross-checks video, audio and keyframe indexes on the
// shared 90 kHz timeline.
package manifest

import (
	"fmt"

	"mediacheck/internal/media"
)

// MaxSkewMicroseconds is the acceptance limit for timeline alignment.
const MaxSkewMicroseconds int64 = 1000

// Alignment summarizes duration/count agreement.
type Alignment struct {
	VideoFrames        int   `json:"video_frames"`
	AudioFrames        int   `json:"audio_frames"`
	IndexEntries       int   `json:"index_entries"`
	VideoDurationTicks int64 `json:"video_duration_ticks"`
	AudioDurationTicks int64 `json:"audio_duration_ticks"`
	IndexDurationTicks int64 `json:"index_duration_ticks"`
	SkewTicks          int64 `json:"skew_ticks"`
	SkewMicroseconds   int64 `json:"manifest_skew_us"`
}

// Manifest is the deterministic delivery sidecar.
type Manifest struct {
	TxID            string                 `json:"tx_id"`
	Codec           string                 `json:"codec"`
	Clock           media.ClockDeclaration `json:"clock"`
	OutputRel       string                 `json:"output_rel"`
	SHA256          string                 `json:"sha256"`
	FrameCount      int                    `json:"frame_count"`
	KeyframeCount   int                    `json:"keyframe_count"`
	TargetBPS       int64                  `json:"target_bps"`
	ActualBPS       int64                  `json:"actual_bps"`
	AudioSampleRate int                    `json:"audio_sample_rate"`
	AudioChannels   int                    `json:"audio_channels"`
	AudioLUFS       float64                `json:"audio_lufs"`
	TruePeakDBFS    float64                `json:"true_peak_dbfs"`
	Alignment       Alignment              `json:"alignment"`
	RecoveredStages []string               `json:"recovered_stages,omitempty"`
	ReencodedVideo  bool                   `json:"video_reencoded"`
}

// AlignDurations checks the maximum pairwise timeline skew.
func AlignDurations(videoTicks, audioTicks, indexTicks int64) (Alignment, error) {
	a := Alignment{
		VideoDurationTicks: videoTicks,
		AudioDurationTicks: audioTicks,
		IndexDurationTicks: indexTicks,
	}
	a.SkewTicks = maxAbsDiff(videoTicks, audioTicks)
	if d := maxAbsDiff(videoTicks, indexTicks); d > a.SkewTicks {
		a.SkewTicks = d
	}
	if d := maxAbsDiff(audioTicks, indexTicks); d > a.SkewTicks {
		a.SkewTicks = d
	}
	// Integer conversion is exact enough for the 1000 us contract.
	a.SkewMicroseconds = a.SkewTicks * 1_000_000 / media.ClockRate
	if a.SkewMicroseconds > MaxSkewMicroseconds {
		return a, media.E(media.StageManifest, media.KindValidation, -1,
			fmt.Errorf("timeline skew %d us exceeds %d us",
				a.SkewMicroseconds, MaxSkewMicroseconds))
	}
	return a, nil
}

// CheckCounts verifies frame/index cardinality after duration alignment.
func (a *Alignment) CheckCounts(videoFrames, audioFrames, indexEntries, keyframes int) error {
	a.VideoFrames = videoFrames
	a.AudioFrames = audioFrames
	a.IndexEntries = indexEntries
	if indexEntries != keyframes {
		return media.E(media.StageManifest, media.KindValidation, -1,
			fmt.Errorf("index entries=%d keyframes=%d", indexEntries, keyframes))
	}
	if indexEntries < 1 {
		return media.E(media.StageManifest, media.KindValidation, -1,
			fmt.Errorf("keyframe index is empty"))
	}
	return nil
}

func maxAbsDiff(a, b int64) int64 {
	if a > b {
		return a - b
	}
	return b - a
}
