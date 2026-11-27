// Package fixture deterministically generates length-prefixed
// RTP/RTCP capture streams for demos and tests. All sample data is
// synthesized in code; no external material is referenced.
package fixture

import (
	"encoding/binary"
	"math"
	"math/rand/v2"
	"sort"

	"archiver/internal/media"
	"archiver/internal/video"
)

const (
	AudioSSRC uint32 = 0x000a0d10
	VideoSSRC uint32 = 0x000b1de0

	AudioRate = 44100
	VideoRate = 90000

	AudioSamplesPerPacket = 882 // 20 ms at 44.1 kHz
	ntpEpochDelta         = 2208988800
	ntpBaseSec            = ntpEpochDelta + 1_700_000_000

	DefaultDurationUs = 12_000_000
)

// TrackSpec describes one generated track.
type TrackSpec struct {
	Kind media.TrackKind
	Rate int
}

// Tracks maps SSRC to track layout for every generated stream.
func Tracks() map[uint32]TrackSpec {
	return map[uint32]TrackSpec{
		AudioSSRC: {Kind: media.TrackAudio, Rate: AudioRate},
		VideoSSRC: {Kind: media.TrackVideo, Rate: VideoRate},
	}
}

// Options control stream generation. Inject names:
// short-header, truncated-extension, duplicate, wrap, reorder, loss,
// late-vfr, bad-frame, drift-no-rtcp.
type Options struct {
	Seed       uint64
	DurationUs int64
	Inject     []string
}

func (o Options) has(name string) bool {
	for _, s := range o.Inject {
		if s == name {
			return true
		}
	}
	return false
}

type record struct {
	buf   []byte
	arrUs int64
	rtp   bool
	video bool
	idx   int
}

func rtpPacket(seq uint16, ts, ssrc uint32, pt byte, payload []byte) []byte {
	b := make([]byte, 12+len(payload))
	b[0] = 0x80
	b[1] = pt
	binary.BigEndian.PutUint16(b[2:4], seq)
	binary.BigEndian.PutUint32(b[4:8], ts)
	binary.BigEndian.PutUint32(b[8:12], ssrc)
	copy(b[12:], payload)
	return b
}

func rtcpSR(ssrc uint32, ntpSec, ntpFrac, rtpTS uint32) []byte {
	b := make([]byte, 28)
	b[0] = 0x80
	b[1] = 200
	binary.BigEndian.PutUint16(b[2:4], 6)
	binary.BigEndian.PutUint32(b[4:8], ssrc)
	binary.BigEndian.PutUint32(b[8:12], ntpSec)
	binary.BigEndian.PutUint32(b[12:16], ntpFrac)
	binary.BigEndian.PutUint32(b[16:20], rtpTS)
	return b
}

func prefix(payload []byte) []byte {
	out := make([]byte, 4+len(payload))
	binary.BigEndian.PutUint32(out[:4], uint32(len(payload)))
	copy(out[4:], payload)
	return out
}

// driftedTicks maps nominal media time to sender RTP ticks.
func driftedTicks(tUs int64, rate int, drift float64) uint32 {
	return uint32(float64(tUs) * float64(rate) * (1 + drift) / 1e6)
}

// audioPCM renders one 20 ms packet of deterministic dual-tone s16le.
func audioPCM(startSample int) []byte {
	b := make([]byte, AudioSamplesPerPacket*2)
	for i := 0; i < AudioSamplesPerPacket; i++ {
		n := startSample + i
		v := 0.15*math.Sin(2*math.Pi*440*float64(n)/AudioRate) +
			0.05*math.Sin(2*math.Pi*880*float64(n)/AudioRate)
		binary.BigEndian.PutUint16(b[2*i:], uint16(int16(v*32767)))
	}
	return b
}

// vfrIntervals is the variable frame rate pattern in microseconds.
var vfrIntervals = []int64{33333, 16667, 50000, 40000, 25000, 41666, 33334}

// Generate builds the capture stream bytes for the given options.
func Generate(o Options) []byte {
	if o.DurationUs <= 0 {
		o.DurationUs = DefaultDurationUs
	}
	rng := rand.New(rand.NewPCG(o.Seed, o.Seed^0x9e3779b9))
	drift := 0.0
	if o.has("drift-no-rtcp") {
		drift = 40e-6
	}
	var recs []record

	// Audio: 20 ms packets.
	seq0 := 0
	if o.has("wrap") {
		seq0 = 65500
	}
	nAudio := int(o.DurationUs / 20_000)
	for i := 0; i < nAudio; i++ {
		t := int64(i) * 20_000
		p := rtpPacket(uint16(seq0+i), driftedTicks(t, AudioRate, drift),
			AudioSSRC, 96, audioPCM(i*AudioSamplesPerPacket))
		recs = append(recs, record{buf: prefix(p), arrUs: t, rtp: true, idx: i})
	}

	// Video: VFR frames, keyframe every 30.
	var pts int64
	var vIdx int
	for pts < o.DurationUs {
		key := vIdx%30 == 0
		payload := make([]byte, 200+rng.IntN(400))
		for i := range payload {
			payload[i] = byte(rng.IntN(256))
		}
		frame := video.EncodeFrame(media.TimeUs(pts), key, payload)
		p := rtpPacket(uint16(vIdx), driftedTicks(pts, VideoRate, drift),
			VideoSSRC, 97, frame)
		recs = append(recs, record{buf: prefix(p), arrUs: pts, rtp: true, video: true, idx: vIdx})
		pts += vfrIntervals[vIdx%len(vfrIntervals)]
		vIdx++
	}

	// RTCP sender reports every 2 s per track. The t=0 reports are
	// ordered before the first media packets so the clock is anchored
	// from the very first conversion.
	if !o.has("drift-no-rtcp") {
		for t := int64(0); t < o.DurationUs; t += 2_000_000 {
			arr := t
			if t == 0 {
				arr = -1
			}
			ntpSec := uint32(ntpBaseSec + t/1_000_000)
			ntpFrac := uint32((t % 1_000_000) << 32 / 1_000_000)
			recs = append(recs, record{buf: prefix(rtcpSR(AudioSSRC, ntpSec, ntpFrac,
				driftedTicks(t, AudioRate, drift))), arrUs: arr})
			recs = append(recs, record{buf: prefix(rtcpSR(VideoSSRC, ntpSec, ntpFrac,
				driftedTicks(t, VideoRate, drift))), arrUs: arr})
		}
	}

	sort.SliceStable(recs, func(i, j int) bool { return recs[i].arrUs < recs[j].arrUs })
	recs = inject(recs, o, rng)

	var out []byte
	for _, r := range recs {
		out = append(out, r.buf...)
	}
	return out
}
