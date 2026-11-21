package fixture

import "mediamerge/internal/mp4util"

// visualSampleEntry builds a VisualSampleEntry with a single child box.
func visualSampleEntry(typ string, width, height uint16, child []byte) []byte {
	body := mp4util.Cat(
		mp4util.Zeros(6),
		mp4util.U16(1),                 // data_reference_index
		mp4util.U16(0), mp4util.U16(0), // pre_defined, reserved
		mp4util.Zeros(12), // pre_defined[3]
		mp4util.U16(width), mp4util.U16(height),
		mp4util.U32(0x00480000), mp4util.U32(0x00480000), // 72 dpi
		mp4util.U32(0),
		mp4util.U16(1), // frame_count
		compressorName("mediamerge"),
		mp4util.U16(0x0018), // depth
		[]byte{0xff, 0xff},
		child,
	)
	return mp4util.Box(typ, body)
}

// audioSampleEntry builds a version-0 AudioSampleEntry with a single child.
func audioSampleEntry(typ string, channels, sampleSize uint16, rate uint32, child []byte) []byte {
	body := mp4util.Cat(
		mp4util.Zeros(6),
		mp4util.U16(1),
		mp4util.U16(0), mp4util.U16(0), // version, revision
		mp4util.U32(0), // vendor
		mp4util.U16(channels), mp4util.U16(sampleSize),
		mp4util.U16(0), mp4util.U16(0), // compression id, packet size
		mp4util.U32(rate<<16),
		child,
	)
	return mp4util.Box(typ, body)
}

func compressorName(name string) []byte {
	out := make([]byte, 32)
	if len(name) > 31 {
		name = name[:31]
	}
	out[0] = byte(len(name))
	copy(out[1:], name)
	return out
}

// avcC builds a plausible H.264 decoder configuration record.
func avcC() []byte {
	sps := []byte{0x67, 0x64, 0x00, 0x1f}
	pps := []byte{0x68, 0xee, 0x3c, 0x80}
	return mp4util.Box("avcC", mp4util.Cat(
		mp4util.U8(1), mp4util.U8(0x64), mp4util.U8(0x00), mp4util.U8(0x1f),
		mp4util.U8(0xff), // lengthSizeMinusOne = 3
		mp4util.U8(0xe1), mp4util.U16(uint16(len(sps))), sps,
		mp4util.U8(1), mp4util.U16(uint16(len(pps))), pps,
	))
}

// esds builds a minimal AAC ES descriptor box.
func esds() []byte {
	asc := []byte{0x12, 0x10}
	decSpecific := mp4util.Cat([]byte{0x05}, descriptorLen(len(asc)), asc)
	decoderConfig := mp4util.Cat(
		[]byte{0x04}, descriptorLen(13+len(decSpecific)),
		mp4util.U8(0x40), mp4util.U8(0x15), mp4util.U24(0),
		mp4util.U32(128000), mp4util.U32(128000),
		decSpecific,
	)
	slConfig := mp4util.Cat([]byte{0x06}, descriptorLen(1), []byte{0x02})
	es := mp4util.Cat(
		[]byte{0x03}, descriptorLen(3+len(decoderConfig)+len(slConfig)),
		mp4util.U16(1), mp4util.U8(0),
		decoderConfig, slConfig,
	)
	return mp4util.FullBox("esds", 0, 0, es)
}

// descriptorLen encodes an MPEG-4 descriptor length (single-byte subset).
func descriptorLen(n int) []byte {
	if n < 0x80 {
		return []byte{byte(n)}
	}
	return []byte{byte(0x80 | (n >> 7)), byte(n & 0x7f)}
}

// alacCookie builds an ALACSpecificConfig box.
func alacCookie(channels uint16, rate uint32) []byte {
	cfg := mp4util.Cat(
		mp4util.U32(4096), // frame_length
		mp4util.U8(0),     // compatible_version
		mp4util.U8(16),    // bit_depth
		mp4util.U8(40), mp4util.U8(10), mp4util.U8(14),
		mp4util.U8(byte(channels)),
		mp4util.U16(255), mp4util.U32(0), mp4util.U32(128000),
		mp4util.U32(rate),
	)
	return mp4util.FullBox("alac", 0, 0, cfg)
}
