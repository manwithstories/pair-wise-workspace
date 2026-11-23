package fixtures

import "encoding/binary"

// box appends an ISOBMFF box (8-byte header) to dst.
func box(dst []byte, typ string, payload ...[]byte) []byte {
	size := 8
	for _, p := range payload {
		size += len(p)
	}
	var hdr [8]byte
	binary.BigEndian.PutUint32(hdr[:4], uint32(size))
	copy(hdr[4:], typ)
	dst = append(dst, hdr[:]...)
	for _, p := range payload {
		dst = append(dst, p...)
	}
	return dst
}

// fullBox is box() with a version and 24-bit flags prefix.
func fullBox(dst []byte, typ string, version byte, flags uint32, payload ...[]byte) []byte {
	head := []byte{version, byte(flags >> 16), byte(flags >> 8), byte(flags)}
	parts := append([][]byte{head}, payload...)
	return box(dst, typ, parts...)
}
