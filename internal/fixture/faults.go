package fixture

import (
	"time"

	"mediacheck/internal/ingest"
)

// StaleIndex returns a copy of the normal index with an expired timestamp.
func StaleIndex(in *ingest.KeyframeIndex) *ingest.KeyframeIndex {
	cp := *in
	cp.ExpiresAt = time.Date(2000, time.January, 1, 0, 0, 0, 0, time.UTC)
	cp.SourceSHA256 = in.SourceSHA256
	return &cp
}

// ClippingPCM returns deterministic full-scale alternating samples.
func ClippingPCM(sampleRate, channels, frames int) []byte {
	if channels <= 0 {
		channels = 1
	}
	out := make([]byte, frames*channels*2)
	for f := 0; f < frames; f++ {
		v := int16(32767)
		if f%2 == 1 {
			v = -32768
		}
		for c := 0; c < channels; c++ {
			i := (f*channels + c) * 2
			out[i] = byte(uint16(v))
			out[i+1] = byte(uint16(v) >> 8)
		}
	}
	return out
}

// SilencePCM returns zero samples.
func SilencePCM(sampleRate, channels, frames int) []byte {
	return make([]byte, frames*channels*2)
}
