package psi

import (
	"hash/crc32"
	"math/bits"
	"testing"
)

func TestCRC32MatchesStdlibReflectedForm(t *testing.T) {
	data, _ := BuildPAT(1, 3, []PATProgram{{Program: 1, PID: 0x100}, {Program: 2, PID: 0x101}})
	reversed := make([]byte, len(data))
	for i, b := range data {
		reversed[i] = bits.Reverse8(b)
	}
	reflected := ^crc32.Update(0, crc32.MakeTable(0xedb88320), reversed)
	if got := bits.Reverse32(reflected); got != CRC32(data) {
		t.Fatalf("stdlib reflected CRC=%#x, custom=%#x", got, CRC32(data))
	}
}
