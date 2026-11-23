package fixture

import (
	"io"

	"mediacore/internal/sample"
)

func writeMinf(w io.Writer, gt genTrack, s Spec, mdatPayloadOff int64) {
	_ = Box("minf", func(w io.Writer) error {
		if gt.spec.Codec == sample.CodecH264 {
			_ = Box("vmhd", func(w io.Writer) error {
				// full box header + 8 bytes graphics mode/opcolor
				u32(w, 0x00000001)
				raw(w, make([]byte, 8))
				return nil
			})(w)
		} else {
			_ = FullBox("smhd", 0, 0, func(w io.Writer) error {
				u16(w, 0)
				u16(w, 0)
				return nil
			})(w)
		}
		_ = Box("dinf", func(w io.Writer) error {
			return FullBox("dref", 0, 0, func(w io.Writer) error {
				u32(w, 1)
				return FullBox("url ", 0, 1, func(w io.Writer) error { return nil })(w)
			})(w)
		})(w)
		writeStbl(w, gt, s, mdatPayloadOff)
		return nil
	})(w)
}

func writeStbl(w io.Writer, gt genTrack, s Spec, mdatPayloadOff int64) {
	_ = Box("stbl", func(w io.Writer) error {
		writeStsd(w, gt)
		writeStts(w, gt)
		writeStsc(w, gt)
		writeStsz(w, gt)
		if s.Mode != ModeMissingIndex && !s.DropStco {
			writeChunkOffsets(w, gt, s, mdatPayloadOff)
		}
		if gt.spec.KeyEvery > 0 {
			writeStss(w, gt)
		}
		return nil
	})(w)
}

func writeStsd(w io.Writer, gt genTrack) {
	_ = FullBox("stsd", 0, 0, func(w io.Writer) error {
		u32(w, 1) // one entry
		switch gt.spec.Codec {
		case "h264":
			writeAvc1(w, gt)
		case "aac":
			writeMp4a(w, gt, true)
		case "opus":
			writeMp4a(w, gt, false)
		default:
			writeUnknown(w, gt)
		}
		return nil
	})(w)
}

func sampleEntryBase(w io.Writer) {
	raw(w, make([]byte, 6)) // reserved
	u16(w, 1)               // data reference index
}

func writeAvc1(w io.Writer, gt genTrack) {
	_ = Box("avc1", func(w io.Writer) error {
		sampleEntryBase(w)
		raw(w, make([]byte, 16)) // predefined+reserved
		u16(w, 320)
		u16(w, 180)
		u32(w, 0x00480000) // horiz resolution 72 dpi
		u32(w, 0x00480000)
		u32(w, 0)
		u16(w, 1) // frame count
		raw(w, make([]byte, 32))
		u16(w, 0x0018) // depth
		u16(w, 0xFFFF)
		// Minimal but valid avcC (SPS/PPS NAL units, synthetic only).
		return Box("avcC", func(w io.Writer) error {
			u8(w, 1)       // configuration version
			u8(w, 66)      // profile
			u8(w, 0)       // compatibility
			u8(w, 30)      // level
			u8(w, 0xFF)    // length size minus one = 3 (4 bytes)
			u8(w, 0xE1)    // one SPS
			sps := []byte{0x67, 0x42, 0x00, 0x1E, 0xAB, 0x40}
			u16(w, uint16(len(sps)))
			raw(w, sps)
			u8(w, 1)
			pps := []byte{0x68, 0xCE, 0x38, 0x80}
			u16(w, uint16(len(pps)))
			raw(w, pps)
			return nil
		})(w)
	})(w)
}

func writeMp4a(w io.Writer, gt genTrack, withESDS bool) {
	_ = Box("mp4a", func(w io.Writer) error {
		sampleEntryBase(w)
		u16(w, 0) // entry version/reserved
		raw(w, make([]byte, 6))
		ch := uint16(2)
		u16(w, ch)
		u16(w, 16) // sample size
		u16(w, 0)
		u16(w, 0)
		u32(w, gt.spec.Timescale<<16) // fixed-point sample rate
		if withESDS {
			return writeESDS(w)
		}
		return writeDOps(w, gt)
	})(w)
}

// writeESDS emits a minimal AudioSpecificConfig (AAC-LC, 2 ch) descriptor tree.
func writeESDS(w io.Writer) error {
	return FullBox("esds", 0, 0, func(w io.Writer) error {
		u8(w, 0x03) // ES_Descriptor tag
		u8(w, 23)   // size
		u16(w, 1)   // ES_ID
		u8(w, 0)
		u8(w, 0x04) // DecoderConfigDescriptor
		u8(w, 15)
		u8(w, 0x40) // audio object type stream
		u8(w, 0x15) // AAC LC + up-stream
		raw(w, make([]byte, 3))
		u32(w, 0)   // max bitrate... buffer size DB (3) + max (4) + avg (4)
		u32(w, 64000)
		u32(w, 64000)
		u8(w, 0x05) // DecoderSpecificInfo
		u8(w, 2)
		u16(w, 0x1210) // ASC: AAC-LC 44.1kHz stereo
		u8(w, 0x06)    // SL config
		u8(w, 1)
		u8(w, 0x02)
		return nil
	})(w)
}

func writeDOps(w io.Writer, gt genTrack) error {
	return Box("dOps", func(w io.Writer) error {
		u8(w, 0) // output gain/coupling header byte
		u8(w, 2) // channels
		u16(w, uint16(gt.spec.Timescale/1000*20)) // pre-skip (synthetic)
		u32(w, gt.spec.Timescale)
		u16(w, 0) // gain
		u8(w, 0)  // channel mapping family 0
		return nil
	})(w)
}

func writeUnknown(w io.Writer, gt genTrack) {
	_ = Box("zzzZ", func(w io.Writer) error {
		sampleEntryBase(w)
		return nil
	})(w)
}
