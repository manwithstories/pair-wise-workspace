package testdata

import (
	"math"
)

// PCMPlan describes a generated audio track with speech and silence.
type PCMPlan struct {
	SampleRate int
	Channels   int
	// Segments alternate by kind; a Kind of "silent" emits zeros, "tone"
	// emits a low-amplitude programme signal and "blip" emits a short loud
	// burst used to prove that short gaps do not split a silence run.
	Segments []PCMSegment
	Seed     int64
}

// PCMSegment is one piece of generated audio.
type PCMSegment struct {
	Kind      string
	Duration  int // milliseconds
	ToneHz    float64
	Amplitude float64 // 0..1, default 0.4 for tone
}

// DefaultPCMPlan yields 1s tone, 1s silence, a 50ms blip inside silence,
// another 1s silence, tone, then silence.
func DefaultPCMPlan() PCMPlan {
	return PCMPlan{
		SampleRate: 48000,
		Channels:   1,
		Segments: []PCMSegment{
			{Kind: "tone", Duration: 1000, ToneHz: 220},
			{Kind: "silent", Duration: 1000},
			{Kind: "blip", Duration: 50, ToneHz: 1000},
			{Kind: "silent", Duration: 1000},
			{Kind: "tone", Duration: 1000, ToneHz: 330},
			{Kind: "silent", Duration: 500},
		},
	}
}

// GeneratePCM renders the plan to interleaved int16 samples.
func GeneratePCM(plan PCMPlan) []int16 {
	if plan.SampleRate <= 0 {
		plan.SampleRate = 48000
	}
	if plan.Channels <= 0 {
		plan.Channels = 1
	}
	var out []int16
	seed := uint64(plan.Seed)*2862933555777941757 + 3037000493
	next := func() float64 {
		seed = seed*6364136223846793005 + 1442695040888963407
		return float64(int64(seed>>11))/float64(1<<52) - 0.5
	}
	phase := 0.0
	for _, seg := range plan.Segments {
		frames := plan.SampleRate * seg.Duration / 1000
		amp := seg.Amplitude
		if amp == 0 && seg.Kind != "silent" {
			amp = 0.4
		}
		if seg.Kind == "blip" {
			amp = 0.5
		}
		for f := 0; f < frames; f++ {
			var v float64
			if seg.Kind != "silent" {
				phase += 2 * math.Pi * seg.ToneHz / float64(plan.SampleRate)
				v = amp * math.Sin(phase)
				v += 0.002 * next() // room tone, below the silence threshold
			}
			s := int16(math.Max(-32768, math.Min(32767, v*32767)))
			for c := 0; c < plan.Channels; c++ {
				out = append(out, s)
			}
		}
	}
	return out
}

// EncodePCM16LE serialises interleaved samples as little-endian bytes, the
// on-disk representation the media pipeline reads.
func EncodePCM16LE(samples []int16) []byte {
	out := make([]byte, len(samples)*2)
	for i, s := range samples {
		out[2*i] = byte(uint16(s))
		out[2*i+1] = byte(uint16(s) >> 8)
	}
	return out
}

// DecodePCM16LE reads little-endian PCM bytes back to samples.
func DecodePCM16LE(data []byte) []int16 {
	n := len(data) / 2
	out := make([]int16, n)
	for i := 0; i < n; i++ {
		out[i] = int16(uint16(data[2*i]) | uint16(data[2*i+1])<<8)
	}
	return out
}

// TimecodeFixture is a list of SMPTE labels that exercises day rollover and
// duplicate timestamps.
type TimecodeFixture struct {
	Rate   string
	Labels []string
	Broken []string
}

// TimecodeFixtures returns the multi-standard timecode cases used by tests.
func TimecodeFixtures() []TimecodeFixture {
	return []TimecodeFixture{
		{Rate: "25", Labels: []string{"23:59:59:24", "23:59:59:24", "00:00:00:00", "00:00:01:00"}},
		{Rate: "30000/1001", Labels: []string{"23:59:59;29", "00:00:00;00", "00:00:00;02"}},
		{Rate: "59.94", Labels: []string{"23:59:59;59", "00:00:00;00"}},
		{Rate: "24", Labels: []string{"00:00:00:00", "00:00:00:23", "00:00:01:00"},
			Broken: []string{"00:00:00:24", "00:00:00", "00:00:0x:00", "24:00:00:00"}},
	}
}
