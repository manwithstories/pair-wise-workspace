package fixture

import (
	"encoding/binary"
	"os"
)

// mp4Timescale gives 1 tick == 1 millisecond, making keyframe gaps in ms
// directly readable from the sample table.
const mp4Timescale uint32 = 1000

// writeMP4 builds a minimal ISO-BMFF proxy containing ftyp/moov with mdhd,
// stts (VFR via alternating sample deltas) and stss (variable keyframe gaps
// given by VFRPattern in base frames), plus an mdat placeholder payload.
func writeMP4(path string, o Options) error {
	// Base frame duration 33 ms with occasional 50 ms frames -> VFR.
	const baseDelta = 33
	const longDelta = 50
	// Approximate the video duration to the audio span (in ms).
	durationMs := o.Frames * 1000 / o.SampleRate
	samples := uint32(durationMs / baseDelta)
	if samples < 8 {
		samples = 8
	}

	runs, totalDelta := buildStts(samples, baseDelta, longDelta)
	keys := buildStss(int(samples), o.VFRPattern)

	stbl := sttsBox(runs)
	if len(keys) > 0 {
		stbl = append(stbl, stssBox(keys)...)
	}
	minf := box("minf", box("stbl", stbl))
	mdia := box("mdia", append(mdhdV0(mp4Timescale, uint32(totalDelta)), minf...))
	moov := box("moov", box("trak", mdia))
	ftyp := box("ftyp", []byte("isom"+"\x00\x00\x00\x00"+"isomavc1mp41"))
	mdat := box("mdat", []byte("fixture-proxy-payload"))

	out := append(append(ftyp, moov...), mdat...)
	return os.WriteFile(path, out, 0o644)
}

// buildStts groups samples into (count, delta) runs. Every 7th sample uses the
// long delta so the table is genuinely VFR.
func buildStts(samples, base, long uint32) ([][2]uint32, uint32) {
	var runs [][2]uint32
	var total uint32
	var i uint32
	for i < samples {
		delta := base
		if i > 0 && i%7 == 0 {
			delta = long
		}
		if n := len(runs); n > 0 && runs[n-1][1] == delta {
			runs[n-1][0]++
		} else {
			runs = append(runs, [2]uint32{1, delta})
		}
		total += delta
		i++
	}
	return runs, total
}

// buildStss turns a repeating gap pattern (in base frames) into 1-based sync
// sample numbers. An empty pattern yields no stss box.
func buildStss(samples int, pattern []int) []uint32 {
	if len(pattern) == 0 {
		return nil
	}
	var keys []uint32
	pos := 1 // 1-based sample number of the first keyframe
	pi := 0
	for pos <= samples {
		keys = append(keys, uint32(pos))
		gap := pattern[pi%len(pattern)]
		pi++
		if gap <= 0 {
			gap = 1
		}
		pos += gap
	}
	return keys
}

// ---- minimal big-endian box writers (fixture-only metadata) ----

func box(typ string, payload []byte) []byte {
	b := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(b[0:], uint32(8+len(payload)))
	copy(b[4:8], typ)
	copy(b[8:], payload)
	return b
}

func fullBox(ver byte, flags uint32) []byte {
	return []byte{ver, byte(flags >> 16), byte(flags >> 8), byte(flags)}
}

func mdhdV0(timescale, duration uint32) []byte {
	p := fullBox(0, 0)
	p = append(p, make([]byte, 8)...)
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, timescale)
	p = append(p, b...)
	binary.BigEndian.PutUint32(b, duration)
	p = append(p, b...)
	p = append(p, 0x55, 0xC4, 0, 0)
	return box("mdhd", p)
}

func sttsBox(runs [][2]uint32) []byte {
	p := fullBox(0, 0)
	c := make([]byte, 4)
	binary.BigEndian.PutUint32(c, uint32(len(runs)))
	p = append(p, c...)
	for _, rd := range runs {
		e := make([]byte, 8)
		binary.BigEndian.PutUint32(e[0:], rd[0])
		binary.BigEndian.PutUint32(e[4:], rd[1])
		p = append(p, e...)
	}
	return box("stts", p)
}

func stssBox(keys []uint32) []byte {
	p := fullBox(0, 0)
	c := make([]byte, 4)
	binary.BigEndian.PutUint32(c, uint32(len(keys)))
	p = append(p, c...)
	for _, k := range keys {
		e := make([]byte, 4)
		binary.BigEndian.PutUint32(e, k)
		p = append(p, e...)
	}
	return box("stss", p)
}
