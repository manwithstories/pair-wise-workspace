package box

import "encoding/binary"

// Pack returns a complete box (header + payload) using a 32-bit size.
// It is used by the offline fixture generator; the analysis path never
// writes boxes.
func Pack(typ string, payload []byte) []byte {
	out := make([]byte, SmallHeaderSize+len(payload))
	binary.BigEndian.PutUint32(out[0:4], uint32(len(out)))
	copy(out[4:8], typ)
	copy(out[SmallHeaderSize:], payload)
	return out
}

// Pack64 returns a complete box using a 64-bit largesize header.
func Pack64(typ string, payload []byte) []byte {
	out := make([]byte, LargeHeaderSize+len(payload))
	binary.BigEndian.PutUint32(out[0:4], 1)
	copy(out[4:8], typ)
	binary.BigEndian.PutUint64(out[8:16], uint64(len(out)))
	copy(out[LargeHeaderSize:], payload)
	return out
}

// PackUUID returns a complete uuid box with the given 16-byte usertype.
func PackUUID(usertype [16]byte, payload []byte) []byte {
	out := make([]byte, SmallHeaderSize+UUIDExtra+len(payload))
	binary.BigEndian.PutUint32(out[0:4], uint32(len(out)))
	copy(out[4:8], "uuid")
	copy(out[8:24], usertype[:])
	copy(out[24:], payload)
	return out
}

// FullPayload prepends the version/flags word of a full box to body.
func FullPayload(version byte, flags uint32, body []byte) []byte {
	out := make([]byte, 4+len(body))
	out[0] = version
	out[1] = byte(flags >> 16)
	out[2] = byte(flags >> 8)
	out[3] = byte(flags)
	copy(out[4:], body)
	return out
}

// PackFull returns a complete full box.
func PackFull(typ string, version byte, flags uint32, body []byte) []byte {
	return Pack(typ, FullPayload(version, flags, body))
}

// SizeOf reports the on-disk size of a box with the given payload size,
// using a 32-bit header. Fixtures use it to pre-compute moov size before
// chunk offsets are known.
func SizeOf(payload uint64) uint64 { return SmallHeaderSize + payload }
