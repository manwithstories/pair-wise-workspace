package fixture

// H.264 unit headers (nal_ref_idc=3 for reference units, type in low 5 bits).
const (
	h264HdrAUD  = byte(0x09)
	h264HdrSPS  = byte(0x67)
	h264HdrPPS  = byte(0x68)
	h264HdrIDR  = byte(0x65)
	h264HdrNonI = byte(0x41)
)

func buildH264(spec StreamSpec, f Fault) ([]byte, []int64, []int64, error) {
	var (
		dst      []byte
		keySeqs  []int64
		keyOffs  []int64
	)
	// Decoder configuration as parameter sets before the first frame.
	dst = appendUnit(dst, true, withTrailer([]byte{h264HdrSPS}, payload(spec.Seed, 0, true, 8)))
	dst = appendUnit(dst, false, withTrailer([]byte{h264HdrPPS}, payload(spec.Seed, 1, true, 4)))

	for fr := 0; fr < spec.Frames; fr++ {
		key := fr%spec.KeyEvery == 0
		auStart := int64(len(dst))
		dst = appendUnit(dst, false, []byte{h264HdrAUD})
		hdr := h264HdrNonI
		if key {
			hdr = h264HdrIDR
			keySeqs = append(keySeqs, int64(fr))
			keyOffs = append(keyOffs, auStart)
		}
		body := withTrailer([]byte{hdr}, payload(spec.Seed, fr, key, spec.ExtraSize))
		dst = appendUnit(dst, false, body)
	}
	return applyFault(dst, f), keySeqs, keyOffs, nil
}

func buildH265(spec StreamSpec, f Fault) ([]byte, []int64, []int64, error) {
	var (
		dst     []byte
		keySeqs []int64
		keyOffs []int64
	)
	// VPS(32)/SPS(33)/PPS(34): header0 = type<<1, header1 = 0.
	for _, t := range []uint8{32, 33, 34} {
		hdr := []byte{t << 1, 0x01}
		dst = appendUnit(dst, true, withTrailer(hdr, payload(spec.Seed, int(t), true, 8)))
	}
	for fr := 0; fr < spec.Frames; fr++ {
		key := fr%spec.KeyEvery == 0
		auStart := int64(len(dst))
		dst = appendUnit(dst, false, []byte{35 << 1, 0x00}) // AUD type 35
		nalType := uint8(0) // TRAIL_N
		if key {
			nalType = 20 // IDR_RADL_N -> treated as IDR_N keyframe
		}
		hdr := []byte{nalType << 1, 0x01}
		if key {
			keySeqs = append(keySeqs, int64(fr))
			keyOffs = append(keyOffs, auStart)
		}
		dst = appendUnit(dst, false, withTrailer(hdr, payload(spec.Seed, fr, key, spec.ExtraSize)))
	}
	return applyFault(dst, f), keySeqs, keyOffs, nil
}

func buildAV1(spec StreamSpec, f Fault) ([]byte, []int64, []int64, error) {
	var (
		dst     []byte
		keySeqs []int64
		keyOffs []int64
	)
	// Sequence header OBU: type=1 in bits 7..3, extension=0, no size field.
	dst = appendUnit(dst, true, withTrailer([]byte{1 << 3}, payload(spec.Seed, 0, true, 8)))
	for fr := 0; fr < spec.Frames; fr++ {
		key := fr%spec.KeyEvery == 0
		auStart := int64(len(dst))
		dst = appendUnit(dst, false, []byte{2 << 3}) // temporal delimiter
		marker := byte(0)
		if key {
			marker = 1
			keySeqs = append(keySeqs, int64(fr))
			keyOffs = append(keyOffs, auStart)
		}
		// Frame OBU (type 6): header then one-byte key marker then payload.
		pl := payload(spec.Seed, fr, key, spec.ExtraSize)
		body := make([]byte, 0, len(pl)+9)
		body = append(body, 6<<3)
		body = append(body, marker)
		body = append(body, pl...)
		body = append(body, trailer(2+len(pl))...)
		dst = appendUnit(dst, false, body)
	}
	return applyFault(dst, f), keySeqs, keyOffs, nil
}

func applyFault(dst []byte, f Fault) []byte {
	if f.TruncateNAL > 0 && int(f.TruncateNAL) < len(dst) {
		dst = dst[:f.TruncateNAL]
	}
	if f.BadHeader >= 0 && int(f.BadHeader) < len(dst) {
		dst[f.BadHeader] |= 0x80
	}
	return dst
}
