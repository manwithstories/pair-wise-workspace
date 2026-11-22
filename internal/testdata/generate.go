package testdata

import (
	"encoding/binary"
	"math"
	"time"

	"mediacq/internal/clock"
)

// rng is a tiny deterministic LCG (Numerical Recipes constants) so fixtures
// never depend on map iteration or the global math/rand source.
type rng struct{ state uint64 }

func newRNG(seed int64) *rng { return &rng{state: uint64(seed)*6364136223846793003 + 1} }

func (r *rng) u32() uint32 {
	r.state = r.state*6364136223846793003 + 1442695040888963407
	return uint32(r.state >> 33)
}

func (r *rng) u32n(n uint32) uint32 {
	if n == 0 {
		return 0
	}
	return r.u32() % n
}

// fill writes a deterministic byte pattern keyed by (track, sampleIndex).
// Audio frames use a high-amplitude little-endian PCM value so they clear any
// realistic silence threshold; silent frames are all-zero.
func fill(buf []byte, trackID uint32, idx int, silent bool) {
	var x uint32 = uint32(trackID)*2654435761 ^ uint32(idx)*40503
	for i := range buf {
		x = x*1103515245 + 12345
		if silent {
			buf[i] = 0
			continue
		}
		if trackID == 2 {
			// PCM audio: place amplitude in the high bytes (little-endian),
			// keeping a deterministic, comfortably audible level (~50%).
			buf[i] = 0
			if i == len(buf)-1 {
				buf[i] = 0x40 ^ byte(x>>24)
			}
			continue
		}
		buf[i] = byte(x>>16) ^ byte(i*37)
	}
}

// BuildVideo constructs the video track spec, including VFR jitter and an
// explicit keyframe pattern. No frame is ever emitted without at least one
// random-access point unless MissingKey is requested (handled by the writer).
func BuildVideo(p Params) TrackSpec {
	r := newRNG(p.Seed + 1)
	base := p.VideoTS / uint32(p.TargetFPS) // ticks per frame, CFR baseline
	// Size frames so the synthetic stream approximates a broadcast bitrate:
	// target bytes/frame = target_bps/8/fps. Deterministic jitter keeps the
	// average close to the target while frames stay distinct.
	bytesPerFrame := p.TargetBitrate / 8 / int64(p.TargetFPS)
	if bytesPerFrame <= 0 {
		bytesPerFrame = 10000
	}
	samples := make([]Sample, p.VideoFrames)
	keyIdx := make([]int, 0, p.VideoFrames/p.KeyframeEvery+1)
	for i := 0; i < p.VideoFrames; i++ {
		d := base
		if p.VFR {
			// Deterministic ±20% jitter, never zero or negative.
			jit := int64(base) / 5
			d = uint32(int64(base) + int64(r.u32n(uint32(2*jit+1))) - int64(jit))
			if d == 0 {
				d = 1
			}
		}
		// Deterministic payload-size jitter of ±5% around the target.
		jitter := int64(float64(bytesPerFrame) * 0.05)
		size := int(bytesPerFrame + int64(r.u32n(uint32(2*jitter+1))) - jitter)
		isKey := p.KeyframeEvery > 0 && i%p.KeyframeEvery == 0
		if isKey {
			keyIdx = append(keyIdx, i)
		}
		payload := make([]byte, size)
		fill(payload, 1, i, false)
		if isKey {
			binary.BigEndian.PutUint32(payload, 0x49445231) // "IDR1" marker
		}
		samples[i] = Sample{DurationTicks: d, Keyframe: isKey, Payload: payload}
	}
	rate, _ := clock.NewRate(int64(p.VideoTS), 1)
	return TrackSpec{
		Kind: KindVideo, TrackID: 1, Timescale: p.VideoTS,
		Samples: samples, Rate: rate,
	}
}

// BuildAudio constructs deterministic PCM frames and records ground-truth
// silent spans on the unified timeline.
func BuildAudio(p Params) (TrackSpec, []clock.Range) {
	r := newRNG(p.Seed + 2)
	frameTicks := p.AudioTS / 100 // 10 ms per PCM frame packet
	samples := make([]Sample, p.AudioSamples)
	silences := make([]clock.Range, 0)
	rate, _ := clock.NewRate(int64(p.AudioTS), 1)
	var tickPos uint64
	inSilence := false
	var silenceStart uint64
	for i := 0; i < p.AudioSamples; i++ {
		silent := p.SilenceEvery > 0 && i%p.SilenceEvery < p.SilenceLen
		payload := make([]byte, p.PCMBytes)
		fill(payload, 2, i, silent)
		samples[i] = Sample{DurationTicks: frameTicks, Keyframe: true, Payload: payload}
		if silent && !inSilence {
			inSilence = true
			silenceStart = tickPos
		}
		if !silent && inSilence {
			inSilence = false
			silences = append(silences, endRange(rate, silenceStart, tickPos))
		}
		tickPos += uint64(frameTicks)
	}
	if inSilence {
		silences = append(silences, endRange(rate, silenceStart, tickPos))
	}
	_ = r
	return TrackSpec{
		Kind: KindAudio, TrackID: 2, Timescale: p.AudioTS,
		Samples: samples, Rate: rate, IsPCM: true,
		Channels: 1, SampleRate: p.AudioTS,
	}, silences
}

func endRange(rate clock.Rate, startTicks, endTicks uint64) clock.Range {
	st, _ := rate.ToTime(int64(startTicks))
	en, _ := rate.ToTime(int64(endTicks))
	return clock.Range{Start: st, End: en}
}

// BuildEvents emits the five metadata families on deliberately different
// timebases. One event is placed exactly on a nominal half-second boundary to
// exercise half-open segment ownership.
func BuildEvents(p Params, totalDur time.Duration) []TimedEvent {
	evs := make([]TimedEvent, 0, 16)
	positions := []time.Duration{
		100 * time.Millisecond,
		500 * time.Millisecond, // exact-boundary event
		900 * time.Millisecond,
		1500 * time.Millisecond,
		2200 * time.Millisecond,
	}
	type fam struct {
		kind Kind
		base uint32
		den  int64
	}
	fams := []fam{
		{KindSubtitle, 1000, 1},   // millisecond clock
		{KindChapter, 1, 1},       // 1 Hz chapter clock
		{KindID3, 90000, 1},       // 90 kHz
		{KindEmsg, 48000, 1},      // audio-rate clock
		{KindSEI, 30000, 1001},    // rational 29.97-ish clock
	}
	id := uint32(1)
	for fi, f := range fams {
		for pi, pos := range positions {
			if pos >= totalDur {
				continue
			}
			rate, _ := clock.NewRate(int64(f.base), f.den)
			// Convert the program-time position into the family's own clock.
			raw, err := rate.FromTime(clock.Time(pos))
			if err != nil {
				panic(err)
			}
			back, err := rate.ToTime(raw)
			if err != nil {
				panic(err)
			}
			evs = append(evs, TimedEvent{
				Kind: f.kind, Time: back,
				Duration: 80 * time.Millisecond, ID: id,
				Timebase: rate, RawTicks: raw, PayloadLen: 16 + fi + pi,
			})
			id++
		}
	}
	return evs
}

// applyDrift returns the sender tick for a reference position given ppm skew.
func applyDrift(nominal int64, ppm float64) int64 {
	return int64(math.Round(float64(nominal) * (1 + ppm/1e6)))
}
