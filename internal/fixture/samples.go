package fixture

import (
	"sort"

	"math/rand"

	"mediqc/internal/media"
)

const (
	videoSamples = 12
	audioSamples = 25
	videoPayload = 9000
	audioPayload = 680
	tickSecond   = 90000
)

func standardTracks() []media.Track {
	return []media.Track{
		{ID: 1, PID: 0x0100, Kind: media.TrackVideo, Codec: media.CodecH264, Timescale: tickSecond},
		{ID: 2, PID: 0x0101, Kind: media.TrackAudio, Codec: media.CodecAAC, Timescale: tickSecond},
	}
}

func makeSamples(seed int64, label string, vfr, wrapClock bool) []media.Sample {
	rng := rand.New(rand.NewSource(seed + int64(hashLabel(label))))
	base := int64(0)
	if wrapClock {
		base = (1 << 33) - 20000
	}
	out := make([]media.Sample, 0, videoSamples+audioSamples)

	videoTime := base
	for i := 0; i < videoSamples; i++ {
		duration := uint32(7500)
		if vfr {
			if i%2 == 0 {
				duration = 6000
			} else {
				duration = 9000
			}
		}
		pts := videoTime
		dts := videoTime
		// One media-time reorder/late event in the VFR fixture.
		if vfr && i == 7 {
			pts = base + 33000
		}
		out = append(out, media.Sample{
			TrackID: 1, PID: 0x0100, Index: i,
			PTS: pts, DTS: dts, Duration: duration,
			Data:         payload(rng, videoPayload, h264Prefix(i)),
			RandomAccess: i == 0 || i%4 == 0,
		})
		videoTime += int64(duration)
	}

	for i := 0; i < audioSamples; i++ {
		pts := base + int64(i)*3600
		out = append(out, media.Sample{
			TrackID: 2, PID: 0x0101, Index: videoSamples + i,
			PTS: pts, DTS: pts, Duration: 3600,
			Data:         payload(rng, audioPayload, adtsPrefix(len(adtsPrefix(0))+audioPayload)),
			RandomAccess: true,
		})
	}
	return out
}

func payload(rng *rand.Rand, n int, prefix []byte) []byte {
	b := make([]byte, n)
	copy(b, prefix)
	if _, err := rng.Read(b[len(prefix):]); err != nil {
		// math/rand source never returns an error; deterministic fallback.
		for i := len(prefix); i < n; i++ {
			b[i] = byte(i * 37)
		}
	}
	return b
}

func h264Prefix(i int) []byte {
	nal := byte(0x65) // IDR slice for deterministic fixtures
	if i%4 != 0 {
		nal = 0x61 // non-IDR slice
	}
	return []byte{0x00, 0x00, 0x00, 0x01, 0x67, 0x42, 0x00, 0x1f, nal}
}

func adtsPrefix(frameLength int) []byte {
	// ADTS header for MPEG-4 LC, 48 kHz, stereo. The CRC-absent header is 7 bytes.
	full := frameLength + 7
	return []byte{
		0xff, 0xf1,
		0x4c, // AAC-LC object type 2, sampling index 3? encoded enough for fixtures
		0x80 | byte((full>>11)&0x03),
		byte((full >> 3) & 0xff),
		byte(((full & 0x07) << 5) | 0x1f),
		0xfc,
	}
}

func hashLabel(s string) uint32 {
	h := uint32(2166136261)
	for _, c := range s {
		h ^= uint32(c)
		h *= 16777619
	}
	return h
}

func interleaveByDecodeTime(in []media.Sample) []media.Sample {
	out := append([]media.Sample(nil), in...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].DTS != out[j].DTS {
			return out[i].DTS < out[j].DTS
		}
		return out[i].TrackID < out[j].TrackID
	})
	return out
}
