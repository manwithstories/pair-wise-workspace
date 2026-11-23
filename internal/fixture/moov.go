package fixture

import (
	"bytes"
	"io"

	"mediacore/internal/sample"
)

// moovSize pre-computes the moov box length so moov-first layouts can place
// mdat correctly. It mirrors buildMoov exactly.
func moovSize(gts []genTrack, variant int, mode Mode) int {
	var buf bytes.Buffer
	_ = buildInto(&buf, gts, Spec{Variant: variant, Mode: mode}, 0)
	return buf.Len()
}

func buildMoov(gts []genTrack, s Spec, mdatPayloadOff int64) []byte {
	var buf bytes.Buffer
	_ = buildInto(&buf, gts, s, mdatPayloadOff)
	return buf.Bytes()
}

func buildInto(buf *bytes.Buffer, gts []genTrack, s Spec, mdatPayloadOff int64) error {
	return WriteBoxes(buf, Box("moov", func(w io.Writer) error {
		writeMvhd(w, gts)
		for i := range gts {
			writeTrak(w, gts[i], i, s, chunkStart(gts, i, mdatPayloadOff))
		}
		return nil
	}))
}

func writeMvhd(w io.Writer, gts []genTrack) {
	_ = FullBox("mvhd", 0, 0, func(w io.Writer) error {
		u32(w, 0) // creation
		u32(w, 0) // modification
		u32(w, 1000)
		var maxDur uint32
		for _, gt := range gts {
			d := uint64(gt.spec.Samples) * uint64(gt.spec.Delta) * 1000 / uint64(gt.spec.Timescale)
			if uint32(d) > maxDur {
				maxDur = uint32(d)
			}
		}
		u32(w, maxDur)
		u32(w, 0x00010000) // rate 1.0
		u16(w, 0x0100)     // volume
		raw(w, make([]byte, 10))
		raw(w, identityMatrix())
		raw(w, make([]byte, 24)) // pre-defined
		u32(w, uint32(len(gts)+1))
		return nil
	})(w)
}

func identityMatrix() []byte {
	m := make([]byte, 36)
	m[0] = 0x00; m[1] = 0x01; m[2] = 0x00; m[3] = 0x00
	m[16] = 0x00; m[17] = 0x01; m[18] = 0x00; m[19] = 0x00
	m[32] = 0x40; m[33] = 0x00; m[34] = 0x00; m[35] = 0x00
	return m
}

func writeTrak(w io.Writer, gt genTrack, idx int, s Spec, chunkOff int64) {
	_ = Box("trak", func(w io.Writer) error {
		writeTkhd(w, gt, idx)
		_ = Box("mdia", func(w io.Writer) error {
			writeMdhd(w, gt)
			writeHdlr(w, gt.spec.Codec)
			writeMinf(w, gt, s, chunkOff)
			return nil
		})(w)
		return nil
	})(w)
}

func writeTkhd(w io.Writer, gt genTrack, idx int) {
	_ = FullBox("tkhd", 0, 3, func(w io.Writer) error { // flags=3 enabled
		u32(w, 0)
		u32(w, 0)
		u32(w, gt.id)
		u32(w, 0) // reserved
		dur := uint64(gt.spec.Samples) * uint64(gt.spec.Delta)
		u32(w, uint32(dur))
		raw(w, make([]byte, 8)) // reserved
		u16(w, 0)               // layer/alt group
		u16(w, 0)
		if gt.spec.Codec == sample.CodecH264 {
			u16(w, 0)
		} else {
			u16(w, 0x0100)
		}
		raw(w, identityMatrix())
		u32(w, 320 << 16)
		u32(w, 180 << 16)
		_ = idx
		return nil
	})(w)
}

func writeMdhd(w io.Writer, gt genTrack) {
	_ = FullBox("mdhd", 0, 0, func(w io.Writer) error {
		u32(w, 0)
		u32(w, 0)
		u32(w, gt.spec.Timescale)
		u32(w, uint32(uint64(gt.spec.Samples)*uint64(gt.spec.Delta)))
		u16(w, 0x55C4) // language 'und'
		u16(w, 0)
		return nil
	})(w)
}

func writeHdlr(w io.Writer, codec sample.Codec) {
	handler := "soun"
	if codec == sample.CodecH264 {
		handler = "vide"
	}
	_ = FullBox("hdlr", 0, 0, func(w io.Writer) error {
		u32(w, 0)
		s4(w, handler)
		raw(w, make([]byte, 12))
		raw(w, []byte(handler+"-handler\x00"))
		return nil
	})(w)
}
