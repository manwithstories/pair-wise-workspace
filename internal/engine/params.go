package engine

// Params are processing parameters covered by the index/cache key.
type Params struct {
	Width          int              `json:"width"`
	Height         int              `json:"height"`
	FPS            int              `json:"fps"`
	BufferSegments int              `json:"buffer_segments"`
	FrameBuffer    int              `json:"frame_buffer"`
	ReviewAround   int              `json:"review_around"`
	AudioGain      map[string]int64 `json:"audio_gain,omitempty"`
}

// DefaultParams returns bounded offscreen render settings.
func DefaultParams() Params {
	return Params{
		Width:          160,
		Height:         120,
		FPS:            8,
		BufferSegments: 1,
		FrameBuffer:    2,
		ReviewAround:   1,
		AudioGain:      map[string]int64{"n": 3, "d": 4},
	}
}
