package sample

import "mediad/internal/silence"

// AudioFixture returns 4 seconds of deterministic 48 kHz stereo S16LE with
// 600 ms and 500 ms quiet runs plus a 40 ms missing interval.
func AudioFixture() ([]byte, []silence.Gap) {
	parts := []struct {
		windows int
		amp     int16
	}{
		{40, 12000}, // 0.8 s loud
		{30, 0},     // 0.6 s silence
		{50, 11000}, // 1.0 s loud
		{25, 0},     // 0.5 s silence threshold
		{50, 13000}, // 1.0 s loud
	}
	var pcm []byte
	for _, p := range parts {
		pcm = append(pcm, silence.TonePCM(p.windows, p.amp)...)
	}
	gaps := []silence.Gap{{
		StartNS: 165 * silence.WindowNS,
		EndNS:   167 * silence.WindowNS, // 40 ms unknown
	}}
	return pcm, gaps
}
