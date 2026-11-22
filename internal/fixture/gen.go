// Package fixture generates deterministic HLS/DASH and packet samples so
// the demo and tests need no external media files.
package fixture

import (
	"fmt"
	"math"
	"math/rand"
	"strings"

	"mediaroute/internal/clock"
	"mediaroute/internal/manifest"
	"mediaroute/internal/normalize"
	"mediaroute/internal/packet"

	"github.com/pion/rtp"
)

// House parameters of the generated programme.
const (
	SegmentCount         = 12
	SegmentDurationTicks = 180000 // 2 s on the 90 kHz base
	TargetKbps           = 2400
	PayloadChunk         = 1376
	framesPerSegment     = 60
	samplesPerFrame      = 4096
)

// targetMSE yields exactly 38.2 dB PSNR for 8-bit samples.
var targetMSE = 255.0 * 255.0 / math.Pow(10, 3.82)

// Segment describes one generated media segment.
type Segment struct {
	Seq            int
	DeclaredStart  uint64 // RTP timestamp of the first packet (may be anomalous)
	DurationTicks  uint64
	PayloadBytes   int64
	Missing        bool // capture node lost the whole segment
	TruncatedFirst bool // first (keyframe) packet flagged truncated
	ContainerColor normalize.ColorMeta
	BitstreamColor normalize.ColorMeta
	SumSqErr       float64
	Samples        uint64
}

// Fixture is a complete generated delivery: manifests, init segment,
// per-segment media parameters and the audio track layout.
type Fixture struct {
	Case     string
	HLS      string
	MPD      []byte
	Init     []byte
	Segments []Segment
	Audio    []manifest.AudioSegment
	House    normalize.AudioTrack
	Source   normalize.AudioTrack
}

// Generate builds the fixture for seed and case ("default", "truncated",
// "slow-consumer"). Generation is fully deterministic per seed.
func Generate(seed int64, caseName string) (*Fixture, error) {
	switch caseName {
	case "default", "slow-consumer", "truncated":
	default:
		return nil, fmt.Errorf("fixture: unknown case %q", caseName)
	}
	rng := rand.New(rand.NewSource(seed))
	fx := &Fixture{Case: caseName}

	truncated := map[int]bool{9: true}
	misaligned := map[int]uint64{}
	if caseName == "truncated" {
		truncated = map[int]bool{2: true, 5: true, 8: true, 9: true}
		misaligned[6] = 7200 // 80 ms off the manifest grid
	}

	// Engineered keyframe offsets: the nine indexed, non-repaired segments
	// of the default case carry deltas summing to exactly 9990 ticks so the
	// mean manifest error is 11.1 ms.
	deltaIdx := []int{0, 1, 2, 4, 5, 6, 8, 10, 11}
	deltas := make([]int64, len(deltaIdx))
	var sum int64
	for i := range deltas {
		deltas[i] = 1110 + int64(rng.Intn(601)) - 300
		sum += deltas[i]
	}
	resid := int64(9990) - sum
	for i := range deltas {
		deltas[i] += resid / int64(len(deltas))
	}
	deltas[0] += resid % int64(len(deltas))
	deltaOf := map[int]int64{}
	for i, seq := range deltaIdx {
		deltaOf[seq] = deltas[i]
	}

	c709 := normalize.ColorMeta{Primaries: "bt709", Transfer: "bt709", Matrix: "bt709"}
	c601 := normalize.ColorMeta{Primaries: "bt601", Transfer: "bt601", Matrix: "smpte170m"}

	indexedBytes := zeroSumJitter(rng, 10, 570000, 5000)
	ib := 0
	for seq := 0; seq < SegmentCount; seq++ {
		start := uint64(seq) * SegmentDurationTicks
		seg := Segment{
			Seq: seq, DurationTicks: SegmentDurationTicks,
			DeclaredStart:  start + uint64(deltaOf[seq]),
			ContainerColor: c709,
		}
		if seq == 3 { // encoder restart: clock jumps backwards
			seg.DeclaredStart = start - SegmentDurationTicks
		}
		if d, ok := misaligned[seq]; ok {
			seg.DeclaredStart = start + d
		}
		switch seq {
		case 7:
			seg.Missing = true
		}
		if truncated[seq] {
			seg.TruncatedFirst = true
		}
		if seq == 5 {
			seg.BitstreamColor = c601 // conflicts with container metadata
		}
		indexed := !seg.Missing && !seg.TruncatedFirst
		if indexed {
			seg.PayloadBytes = indexedBytes[ib]
			ib++
			seg.Samples = framesPerSegment * samplesPerFrame
			jitter := 1 + (rng.Float64()-0.5)*0.04
			seg.SumSqErr = targetMSE * float64(seg.Samples) * jitter
		} else {
			seg.PayloadBytes = 570000
		}
		fx.Segments = append(fx.Segments, seg)
	}
	// Normalise squared-error sums to the exact PSNR target.
	var totSE, totSamples float64
	for _, s := range fx.Segments {
		totSE += s.SumSqErr
		totSamples += float64(s.Samples)
	}
	scale := targetMSE * totSamples / totSE
	for i := range fx.Segments {
		fx.Segments[i].SumSqErr *= scale
	}

	for seq := 0; seq < SegmentCount; seq++ {
		fx.Audio = append(fx.Audio, manifest.AudioSegment{
			Seq: seq, Samples: 96000, Rate: clock.Rate48000,
		})
	}
	fx.House = normalize.AudioTrack{Rate: clock.Rate48000, Layout: normalize.Stereo}
	fx.Source = normalize.AudioTrack{Rate: clock.Rate44100, Layout: normalize.Mono}

	fx.HLS = buildHLS()
	fx.MPD = []byte(buildMPD())
	init, err := buildInit()
	if err != nil {
		return nil, err
	}
	fx.Init = init
	return fx, nil
}

// zeroSumJitter returns n values around base whose sum is exactly n*base.
func zeroSumJitter(rng *rand.Rand, n int, base, spread int64) []int64 {
	out := make([]int64, n)
	var sum int64
	for i := range out {
		out[i] = base + rng.Int63n(2*spread+1) - spread
		sum += out[i]
	}
	resid := base*int64(n) - sum
	for i := range out {
		out[i] += resid / int64(n)
	}
	out[0] += resid % int64(n)
	return out
}

// SegmentBytes materialises one segment's framed RTP packet stream.
// At most one segment is materialised at a time, bounded by PayloadBytes.
func (fx *Fixture) SegmentBytes(seq int) []byte {
	seg := fx.Segments[seq]
	if seg.Missing {
		return nil
	}
	rng := rand.New(rand.NewSource(int64(seq)*7919 + 17))
	n := int((seg.PayloadBytes + PayloadChunk - 1) / PayloadChunk)
	var out []byte
	ts := uint32(seg.DeclaredStart)
	left := seg.PayloadBytes
	for i := 0; i < n; i++ {
		chunk := int64(PayloadChunk)
		if chunk > left {
			chunk = left
		}
		payload := make([]byte, chunk)
		rng.Read(payload)
		pkt := rtp.Packet{
			Header:  rtp.Header{Version: 2, SequenceNumber: uint16(i), Timestamp: ts, SSRC: 0x14965},
			Payload: payload,
		}
		raw, err := pkt.Marshal()
		if err != nil {
			panic(err)
		}
		var flags uint8
		if i == 0 {
			flags |= packet.FlagKeyframe
			if seg.TruncatedFirst {
				flags |= packet.FlagTruncated
			}
		}
		out = append(out, packet.EncodeFrame(raw, flags)...)
		ts += 441
		left -= chunk
	}
	return out
}

func buildHLS() string {
	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-TARGETDURATION:2\n#EXT-X-MEDIA-SEQUENCE:0\n")
	for seq := 0; seq < SegmentCount; seq++ {
		fmt.Fprintf(&b, "#EXTINF:2.0,\nseg%02d.ts\n", seq)
	}
	b.WriteString("#EXT-X-ENDLIST\n")
	return b.String()
}

func buildMPD() string {
	return fmt.Sprintf(`<MPD><Period>
<AdaptationSet contentType="video"><Representation id="v1" bandwidth="%d" colorPrimaries="bt709">
<SegmentList timescale="90000" duration="%d" count="%d"/></Representation></AdaptationSet>
<AdaptationSet contentType="audio"><Representation id="a1" bandwidth="128000" audioSamplingRate="48000" audioChannels="2">
<SegmentList timescale="90000" duration="%d" count="%d"/></Representation></AdaptationSet>
</Period></MPD>`,
		(TargetKbps-200)*1000, SegmentDurationTicks, SegmentCount, SegmentDurationTicks, SegmentCount)
}
