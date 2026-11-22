package main

import (
	"math/rand"

	"mediasim/internal/meta"
	"mediasim/internal/rtp"
)

const (
	audioRate            = 48000
	videoRate            = 90000
	audioFrames          = 150 // 3 s at 20 ms per frame
	videoFrames          = 90  // 3 s at ~33.3 ms per frame
	audioSamplesPerFrame = 960
	videoTicksPerFrame   = 3000
	audioBaseSeq         = 65530 // crosses the 16-bit sequence wrap
	videoBaseSeq         = 1000
	videoBaseTS          = 0xFFFFF000 // crosses the 32-bit timestamp wrap
	streamMediaTicks     = 270000     // 3 s on the 90 kHz timeline
	segmentTicks         = 45000      // 500 ms commit segments
)

// frame is one generated media frame before packetisation.
type frame struct {
	seq     uint16
	ts      uint32
	media   int64 // nominal rebased media time in 90 kHz ticks
	payload []byte
}

// stream is one generated track.
type stream struct {
	track  string
	rate   uint32
	frames []frame
}

func payloadFor(trackIdx int, seq uint16, n int) []byte {
	p := make([]byte, n)
	for j := range p {
		p[j] = byte(int(seq)*31 + j*7 + trackIdx)
	}
	return p
}

// genStreams builds the synthetic audio and video tracks. Video runs
// ~50 ppm fast with bounded per-frame interval jitter; audio is exact.
func genStreams(rng *rand.Rand) (audio, video *stream) {
	audio = &stream{track: "audio", rate: audioRate}
	for i := 0; i < audioFrames; i++ {
		seq := uint16(audioBaseSeq + i)
		audio.frames = append(audio.frames, frame{
			seq:     seq,
			ts:      uint32(i * audioSamplesPerFrame),
			media:   int64(i) * 1800,
			payload: payloadFor(1, seq, 8),
		})
	}
	video = &stream{track: "video", rate: videoRate}
	jitter := 0
	for i := 0; i < videoFrames; i++ {
		jitter += rng.Intn(61) - 30
		if jitter > 150 {
			jitter = 150
		}
		if jitter < -150 {
			jitter = -150
		}
		seq := uint16(videoBaseSeq + i)
		nominal := int64(i)*videoTicksPerFrame + int64(i)*15/100 // ~50 ppm fast
		video.frames = append(video.frames, frame{
			seq:     seq,
			ts:      uint32(videoBaseTS + i*videoTicksPerFrame + i*15/100 + jitter),
			media:   nominal + int64(jitter),
			payload: payloadFor(2, seq, 16),
		})
	}
	return audio, video
}

// wirePacket is one RTP packet on the wire with its arrival time.
type wirePacket struct {
	track   string
	data    []byte
	arrival int64 // media ticks
}

// genWire packetises a stream and applies channel faults: independent
// loss, one burst loss, adjacent reordering and duplication. The first
// and last two packets are never dropped (stream edges are a distinct
// fault class). All randomness comes from rng.
func genWire(s *stream, rng *rand.Rand, loss float64, burst int) []wirePacket {
	n := len(s.frames)
	dropped := make([]bool, n)
	for i := 2; i < n-2; i++ {
		if rng.Float64() < loss {
			dropped[i] = true
		}
	}
	if burst > 0 && n > burst+4 {
		start := 2 + rng.Intn(n-4-burst)
		for i := start; i < start+burst; i++ {
			dropped[i] = true
		}
	}
	var order []int
	for i := 0; i < n; i++ {
		if !dropped[i] {
			order = append(order, i)
		}
	}
	for i := 0; i+1 < len(order); i++ {
		if rng.Float64() < 0.10 {
			order[i], order[i+1] = order[i+1], order[i]
			i++
		}
	}
	var out []wirePacket
	var prevPayload []byte
	for _, idx := range order {
		f := s.frames[idx]
		wire, err := rtp.Marshal(f.seq, f.ts, uint32(s.rate), f.payload, prevPayload)
		if err != nil {
			panic(err)
		}
		arrival := f.media + int64(rng.Intn(180))
		out = append(out, wirePacket{track: s.track, data: wire, arrival: arrival})
		if rng.Float64() < 0.05 {
			out = append(out, wirePacket{track: s.track, data: wire, arrival: arrival + 1})
		}
		prevPayload = f.payload
	}
	return out
}

// genEvents builds the four timed-metadata classes plus emsg events.
// Every fourth event arrives late. All timestamps are deterministic.
func genEvents() ([]meta.Event, []meta.EmsgEvent) {
	var events []meta.Event
	add := func(kind meta.Kind, track string, rawTS uint32, media int64, payload string, late bool) {
		arrival := media - 900
		if late {
			arrival = media + 2700
		}
		events = append(events, meta.Event{
			Kind: kind, Track: track, RawTS: rawTS,
			Arrival: arrival, Payload: payload,
		})
	}
	k := 0
	for m := int64(0); m < streamMediaTicks; m += 45000 {
		add(meta.Subtitle, "meta", uint32(m), m, "subtitle", k%4 == 3)
		k++
	}
	for m := int64(0); m < streamMediaTicks; m += 90000 {
		add(meta.Chapter, "meta", uint32(m), m, "chapter", k%4 == 3)
		k++
	}
	for m := int64(0); m < streamMediaTicks; m += 67500 {
		add(meta.ID3, "audio", uint32(m*48000/90000), m, "id3", k%4 == 3)
		k++
	}
	for m := int64(15000); m < streamMediaTicks; m += 30000 {
		add(meta.SEI, "meta", uint32(m), m, "sei", k%4 == 3)
		k++
	}
	var emsg []meta.EmsgEvent
	for i, m := 0, int64(0); m < streamMediaTicks; i, m = i+1, m+22500 {
		emsg = append(emsg, meta.EmsgEvent{
			SchemeID:         "urn:mediasim:emsg",
			Value:            "v1",
			Timescale:        90000,
			PresentationTime: uint64(m),
			Duration:         22500,
			ID:               uint32(i + 1),
			Data:             []byte("emsg-payload"),
		})
	}
	return events, emsg
}
