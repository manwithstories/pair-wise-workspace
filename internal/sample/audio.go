package sample

import (
	"encoding/binary"
	"math"
)

// tone describes one deterministic channel source.
type tone struct {
	freq float64
	amp  float64
	kind int
}

func channelTones(seed int64) []tone {
	// Seed only perturbs musical frequencies slightly; output remains fully
	// deterministic for a fixed --seed.
	jitter := float64(seed%7) * 0.5
	return []tone{
		{freq: 220.0 + jitter, amp: 0.34, kind: 0},  // L
		{freq: 277.18 + jitter, amp: 0.32, kind: 0}, // R
		{freq: 440.0, amp: 0.24, kind: 1},           // C pulsed
		{freq: 55.0, amp: 0.30, kind: 0},            // LFE
		{freq: 329.63, amp: 0.14, kind: 2},          // Ls arpeggio
		{freq: 392.0, amp: 0.14, kind: 2},           // Rs arpeggio
	}
}

// sampleAt generates a bounded deterministic 5.1 waveform. The final section is
// exact silence, giving loudness tests an explicit "do not boost noise" region.
func sampleAt(globalFrame, rate, channels int, seed int64, seconds int) []float64 {
	out := make([]float64, channels)
	t := float64(globalFrame) / float64(rate)
	totalFrames := seconds * rate
	if globalFrame > totalFrames-rate/2 { // final 0.5 s is silent
		return out
	}
	tones := channelTones(seed)
	for ch := 0; ch < channels && ch < len(tones); ch++ {
		tn := tones[ch]
		v := tn.amp * math.Sin(2*math.Pi*tn.freq*t)
		switch tn.kind {
		case 1: // 2 Hz center pulse
			gate := math.Sin(2 * math.Pi * 2.0 * t)
			if gate < 0 {
				gate = 0
			}
			v *= gate * gate
		case 2: // eighth-note-ish arpeggio envelope
			phase := math.Mod(t*4.0, 1.0)
			env := math.Exp(-5.0 * phase)
			v *= env
		}
		// A short fade in/out avoids artificial edge clicks.
		fadeFrames := rate / 100
		if globalFrame < fadeFrames {
			v *= float64(globalFrame) / float64(fadeFrames)
		}
		if globalFrame > totalFrames-fadeFrames {
			v *= float64(totalFrames-globalFrame) / float64(fadeFrames)
		}
		if v > 0.95 {
			v = 0.95
		} else if v < -0.95 {
			v = -0.95
		}
		out[ch] = v
	}
	return out
}

func encodeL16(frame, packetFrames, channels, rate int, seed int64, seconds int) []byte {
	payload := make([]byte, packetFrames*channels*2)
	for f := 0; f < packetFrames; f++ {
		global := frame*packetFrames + f
		samples := sampleAt(global, rate, channels, seed, seconds)
		for ch := 0; ch < channels; ch++ {
			x := 0.0
			if ch < len(samples) {
				x = samples[ch]
			}
			v := int16(math.Round(x * 32767))
			off := (f*channels + ch) * 2
			binary.BigEndian.PutUint16(payload[off:off+2], uint16(v))
		}
	}
	return payload
}
