package fixture

import (
	"encoding/binary"
	"hash/crc32"
)

// SampleOverhead is header size excluding body and trailing CRC.
const SampleOverhead = 8 + 4 // 8-byte header + 4-byte CRC trailer

// lcg is a deterministic, dependency-free byte generator. Two assets with
// the same seed produce byte-identical samples, which is what makes the
// manifest SHA-256 reproducible across reruns.
type lcg struct{ state uint64 }

func newLCG(seed int64, stream uint32, index uint32) *lcg {
	return &lcg{state: uint64(seed)*1_000_003_007 + uint64(stream)*97_171 + uint64(index)*31}
}

func (g *lcg) next() byte {
	// MMIX constants; full 64-bit period.
	g.state = g.state*6_364_136_223_846_793_005 + 1_442_695_040_888_963_407
	return byte(g.state >> 56)
}

// BuildSample returns one checksummed synthetic sample for the given
// 0-based track-local index. stream distinguishes video (1) from audio (2).
func BuildSample(seed int64, stream, index uint32, bodyLen int) []byte {
	buf := make([]byte, SampleOverhead+bodyLen)
	buf[0] = 'M'
	buf[1] = 'C'
	putU32(buf[2:], index)
	putU16(buf[6:], uint16(bodyLen))
	g := newLCG(seed, stream, index)
	for i := 0; i < bodyLen; i++ {
		buf[8+i] = g.next()
	}
	putU32(buf[8+bodyLen:], crc32.ChecksumIEEE(buf[:8+bodyLen]))
	return buf
}

// VerifySample validates the magic, declared index and CRC of a sample
// read back from disk. It returns the body length on success.
func VerifySample(raw []byte, wantIndex uint32) (int, error) {
	if len(raw) < SampleOverhead {
		return 0, ErrSampleTooShort
	}
	if raw[0] != 'M' || raw[1] != 'C' {
		return 0, ErrBadMagic
	}
	gotIndex := binary.BigEndian.Uint32(raw[2:])
	if gotIndex != wantIndex {
		return 0, ErrIndexMismatch
	}
	bodyLen := int(binary.BigEndian.Uint16(raw[6:]))
	if len(raw) != SampleOverhead+bodyLen {
		return 0, ErrLengthMismatch
	}
	want := binary.BigEndian.Uint32(raw[len(raw)-4:])
	if crc32.ChecksumIEEE(raw[:len(raw)-4]) != want {
		return 0, ErrCRCMismatch
	}
	return bodyLen, nil
}
