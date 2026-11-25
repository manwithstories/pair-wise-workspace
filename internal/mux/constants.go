package mux

import "mediacheck/internal/media"

const (
	// Timescale is the shared media time base.
	Timescale = media.ClockRate
	// TargetAudioRate is the normalized PCM sample rate.
	TargetAudioRate = 48000
)
