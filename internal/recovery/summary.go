package recovery

// Summary is the machine-readable result written to output/summary.json.
type Summary struct {
	OK                bool    `json:"ok"`
	Segments          int     `json:"segments"`
	SegmentSHA256     int     `json:"segment_sha256"`
	RoundtripMatched  int     `json:"roundtrip_matched"`
	RoundtripTotal    int     `json:"roundtrip_total"`
	RoundtripRate     float64 `json:"roundtrip_rate"`
	KeyframeMatched   int     `json:"keyframe_matched"`
	KeyframeTotal     int     `json:"keyframe_total"`
	KeyframeHit       float64 `json:"keyframe_hit"`
	ReplayMatch       bool    `json:"replay_match"`
	ResumeMatch       bool    `json:"resume_match"`
	OutputSampleRate  int     `json:"output_sr"`
	Channels          int     `json:"channels"`
	IntegratedLUFS    float64 `json:"integrated_lufs"`
	TruePeakDBTP      float64 `json:"true_peak_dbtp"`
	TargetLUFS        float64 `json:"target_lufs"`
	MasterOutputSHA   string  `json:"master_output_sha256"`
	ConfigHash        string  `json:"config_hash"`
	CheckpointKey     string  `json:"checkpoint_key"`
	ProcessedSegments int     `json:"processed_segments"`
	ReusedSegments    int     `json:"reused_segments"`
	RTPWrapped        bool    `json:"rtp_wrapped"`
	RTPExtSequence    uint64  `json:"rtp_ext_sequence"`
	RTPExtTimestamp   uint64  `json:"rtp_ext_timestamp"`
	Periods           int     `json:"periods"`
	DamageCode        string  `json:"damage_code,omitempty"`
}

// RTPConfig controls the wrapper test vector used by the pipeline.
type RTPConfig struct {
	ClockRate     int64  `json:"clock_rate"`
	BaseSequence  uint16 `json:"base_sequence"`
	BaseTimestamp uint32 `json:"base_timestamp"`
	FrameTicks    int64  `json:"frame_ticks"`
}

// RunOptions configures one pipeline execution.
type RunOptions struct {
	Root              string
	Params            Params
	RTP               RTPConfig
	ExpectedKeyframes []int
	Resume            bool
}
