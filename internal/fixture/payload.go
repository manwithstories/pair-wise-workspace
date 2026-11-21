package fixture

import "encoding/binary"

// Payload returns the deterministic bytes for one sample of one track.
//
// The pattern is derived from the run seed, the track ID and the sample
// index, never from generation order, so a verifier can regenerate exactly
// the bytes a given sample must contain without replaying the writer. That is
// what makes the quality metric reproducible across runs.
func Payload(seed int64, trackID uint32, sampleIndex int, size int, dst []byte) []byte {
	if cap(dst) < size {
		dst = make([]byte, size)
	}
	dst = dst[:size]
	// splitmix64 seeded per sample gives a stable, well-distributed stream.
	x := uint64(seed)*0x9e3779b97f4a7c15 ^ uint64(trackID)<<32 ^ uint64(sampleIndex+1)*0xbf58476d1ce4e5b9
	var buf [8]byte
	for i := 0; i < size; i += 8 {
		x += 0x9e3779b97f4a7c15
		z := x
		z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
		z = (z ^ (z >> 27)) * 0x94d049bb133111eb
		z ^= z >> 31
		binary.LittleEndian.PutUint64(buf[:], z)
		copy(dst[i:], buf[:])
	}
	return dst
}
