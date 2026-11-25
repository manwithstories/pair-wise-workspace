package ebml

// EncodeID encodes an element ID using its natural big-endian width.
func EncodeID(id uint32) []byte {
	switch {
	case id <= 0xFF:
		return []byte{byte(id)}
	case id <= 0xFFFF:
		return []byte{byte(id >> 8), byte(id)}
	case id <= 0xFFFFFF:
		return []byte{byte(id >> 16), byte(id >> 8), byte(id)}
	default:
		return []byte{byte(id >> 24), byte(id >> 16), byte(id >> 8), byte(id)}
	}
}

// EncodeSize encodes a known EBML payload size in its shortest VINT form.
func EncodeSize(size uint64) []byte {
	for length := 1; length <= MaxVINTBytes; length++ {
		dataBits := uint(7 * length)
		unknown := uint64(1)<<dataBits - 1
		if size < unknown {
			marker := uint64(1) << dataBits
			v := marker | size
			out := make([]byte, length)
			for i := length - 1; i >= 0; i-- {
				out[i] = byte(v & 0xFF)
				v >>= 8
			}
			return out
		}
	}
	// Values above int64 payload limits are not produced by this project.
	return UnknownSize(MaxVINTBytes)
}

// UnknownSize returns the reserved unknown-size VINT of the requested width.
func UnknownSize(length int) []byte {
	if length < 1 || length > MaxVINTBytes {
		length = MaxVINTBytes
	}
	out := make([]byte, length)
	marker := uint64(1) << uint(7*length)
	v := marker | (marker - 1)
	for i := length - 1; i >= 0; i-- {
		out[i] = byte(v & 0xFF)
		v >>= 8
	}
	return out
}

// ElementHeader returns a complete known-size element header.
func ElementHeader(id uint32, payloadSize int) []byte {
	out := EncodeID(id)
	return append(out, EncodeSize(uint64(payloadSize))...)
}

// AppendElement appends a known-size element to dst.
func AppendElement(dst []byte, id uint32, payload []byte) []byte {
	dst = append(dst, ElementHeader(id, len(payload))...)
	return append(dst, payload...)
}

// BuildElement builds a known-size element.
func BuildElement(id uint32, payload []byte) []byte {
	return AppendElement(nil, id, payload)
}

// UnknownHeader builds an ID plus an unknown-length size VINT.
func UnknownHeader(id uint32, length int) []byte {
	out := EncodeID(id)
	return append(out, UnknownSize(length)...)
}

// EncodeUint encodes an unsigned integer in its shortest EBML form.
func EncodeUint(v uint64) []byte {
	if v == 0 {
		return []byte{0}
	}
	var buf [8]byte
	n := 8
	for v > 0 {
		n--
		buf[n] = byte(v & 0xFF)
		v >>= 8
	}
	return buf[n:]
}

// UintElement builds an unsigned integer element.
func UintElement(id uint32, v uint64) []byte {
	return BuildElement(id, EncodeUint(v))
}

// StringElement builds a UTF-8 string element.
func StringElement(id uint32, s string) []byte {
	return BuildElement(id, []byte(s))
}
