// Package synth generates deterministic sample captures for demos and tests.
// Everything is produced from a seed; no external files are read.
package synth

import (
	"fmt"
	"math"
	"math/rand"

	"medpipe/internal/model"
)

// Options controls the generated capture.
type Options struct {
	Seed     int64
	Tracks   int
	Frames   int
	DriftPPM float64
	WrapAt   uint64 // counter modulus; 0 disables wrapping (64-bit)
	VFR      bool
}

// Capture is a generated set of tracks and their frames.
type Capture struct {
	Tracks []model.Track
	Frames map[string][]model.Frame
}

// Generate builds a capture. Video tracks carry small synthetic payloads,
// audio tracks carry per-frame energy with deliberate silent stretches.
func Generate(opt Options) (*Capture, error) {
	if opt.Tracks <= 0 {
		return nil, model.New(model.KindInput, "synth.Generate", "track count must be positive")
	}
	if opt.Frames <= 0 {
		return nil, model.New(model.KindInput, "synth.Generate", "frame count must be positive")
	}
	bits := uint8(64)
	if opt.WrapAt > 0 {
		switch {
		case opt.WrapAt <= 256:
			bits = 8
		case opt.WrapAt <= 65536:
			bits = 16
		case opt.WrapAt <= 1<<32:
			bits = 32
		default:
			bits = 64
		}
	}
	rng := rand.New(rand.NewSource(opt.Seed))
	c := &Capture{Frames: make(map[string][]model.Frame, opt.Tracks)}
	for t := 0; t < opt.Tracks; t++ {
		kind := model.KindVideo
		idPrefix := "cam"
		timescale := uint32(90000)
		nominal := uint64(3000) // 30fps at 90kHz
		stepTicks := 3000.0
		if t%3 == 2 {
			kind = model.KindAudio
			idPrefix = "mic"
			timescale = 48000
			nominal = 1024
			stepTicks = 1024.0
		}
		id := fmt.Sprintf("%s%d", idPrefix, t)
		origin := int64(rng.Intn(40_000_000)) // jittered capture start
		tr := model.Track{
			ID: id, Kind: kind, Timescale: timescale, ClockBits: bits,
			NominalTickDelta: nominal, OriginNS: origin,
			Extra: []model.Metadata{{Key: "source", Value: id}},
		}
		if kind == model.KindAudio {
			tr.SampleRate = 48000
		}
		// Per-track drift differs so cross-track correction is exercised.
		drift := opt.DriftPPM
		if drift != 0 {
			drift += float64(t*17) - 20
		}
		rate := stepTicks * (1 + drift/1e6)
		var tick float64
		var mod uint64
		if opt.WrapAt > 0 {
			mod = opt.WrapAt
		}
		frames := make([]model.Frame, opt.Frames)
		for i := 0; i < opt.Frames; i++ {
			if opt.VFR && i > 0 {
				// Every 7th frame takes a longer gap, every 11th a shorter one,
				// which is typical of a camera dropping into a lower rate.
				switch {
				case i%7 == 0:
					tick += rate * 1.5
				case i%11 == 0:
					tick += rate * 0.75
				default:
					tick += rate
				}
			} else if i > 0 {
				tick += rate
			}
			raw := uint64(math.Round(tick))
			if mod > 0 {
				raw %= mod
			}
			fr := model.Frame{
				TrackID: id, Seq: uint64(i), RawTS: raw,
				Keyframe: i%12 == 0,
				Meta:     []model.Metadata{{Key: "seq", Value: fmt.Sprint(i)}},
			}
			if kind == model.KindVideo {
				fr.Width, fr.Height = 1920, 1080
				fr.Payload = model.EncodeVideoPayload(fr.Width, fr.Height, uint32(i), payloadFor(opt.Seed, t, i))
			} else {
				fr.Energy = energyFor(opt.Seed, t, i)
				fr.Payload = model.EncodeAudioPayload(uint32(i), uint16(len(fr.Energy)), []byte{byte(i), byte(t)})
			}
			fr.Checksum = fr.IntegrityChecksum()
			frames[i] = fr
		}
		c.Tracks = append(c.Tracks, tr)
		c.Frames[id] = frames
	}
	return c, nil
}

// payloadFor produces content that occasionally repeats so dedupe has real
// duplicates to find: frames 3,7,11,... reuse the payload of the frame before
// them, while every other frame is fresh. The payload is a pure function of
// (seed, track, source frame) so the entire capture is reproducible.
func payloadFor(seed int64, track, i int) []byte {
	src := i
	if i%4 == 3 && i > 0 {
		src = i - 1
	}
	n := 24 + src%17
	buf := make([]byte, n)
	r := rand.New(rand.NewSource(seed*1_000_003 + int64(track)*7_919 + int64(src)))
	r.Read(buf)
	return buf
}

// energyFor creates 25ms energy blocks in 1s frames: a loud body with two
// silent stretches per second.
func energyFor(seed int64, track, i int) []float64 {
	const blocks = 4
	out := make([]float64, blocks)
	// 50 frames per second at 20ms steps: two 150ms quiet stretches.
	phase := i % 40
	quiet := (phase >= 10 && phase < 17) || (phase >= 28 && phase < 35)
	for b := 0; b < blocks; b++ {
		if quiet {
			out[b] = 0.001 * float64(b%2)
			continue
		}
		out[b] = 0.2 + 0.6*math.Abs(math.Sin(float64(seed+int64(track*7+i*3+b)))/10)
	}
	return out
}
