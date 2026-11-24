package ebml

// Test-side minimal EBML writer used to build deterministic fixtures.

// encVInt encodes v as an EBML variable-length integer. With width <= 0 the
// shortest encodable width is chosen.
func encVInt(v uint64, width int) []byte {
	if width <= 0 {
		for w := 1; w <= 8; w++ {
			if v < (uint64(1)<<uint(7*w))-1 {
				width = w
				break
			}
		}
	}
	out := make([]byte, width)
	for i := width - 1; i >= 0; i-- {
		out[i] = byte(v & 0xFF)
		v >>= 8
	}
	out[0] |= 1 << uint(8-width)
	return out
}

// encID encodes an element ID, whose numeric value already includes the
// VINT length-marker bits (e.g. Segment is 0x18538067).
func encID(id uint32) []byte {
	for _, w := range []int{1, 2, 3, 4} {
		if id < uint32(1)<<uint(7*w+1) {
			return encVInt(uint64(id), w)
		}
	}
	panic("id too wide")
}

// elem wraps payload in a sized element.
func elem(id uint32, payload []byte) []byte {
	return append(append(encID(id), encVInt(uint64(len(payload)), 0)...), payload...)
}

// elemUnknown emits an element whose data size is the reserved unknown
// value. Per RFC 9559 such an element must be its parent's last child.
func elemUnknown(id uint32, payload []byte) []byte {
	return append(append(encID(id), 0xFF), payload...)
}
