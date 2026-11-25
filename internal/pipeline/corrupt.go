package pipeline

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"avcheck/internal/ebml"
)

// truncateVint cuts the file inside the size vint of the first cluster, which
// the parser must report as a truncated variable-length integer at that exact
// offset rather than a generic end-of-file.
func truncateVint(data []byte) ([]byte, int64, error) {
	off := bytes.Index(data, idBytes(ebml.IDCluster))
	if off < 0 {
		return nil, 0, fmt.Errorf("corrupt: cluster not found")
	}
	// Cluster ID is 4 bytes; the fixture writes an 8-byte size vint after it.
	cut := off + 4 + 3
	if cut >= len(data) {
		return nil, 0, fmt.Errorf("corrupt: cluster too close to end")
	}
	return data[:cut], int64(cut), nil
}

// oversizeElement rewrites a cluster's size vint to a value beyond the hard
// element cap, proving the parser refuses the allocation instead of trying.
func oversizeElement(data []byte) ([]byte, int64, error) {
	off := bytes.Index(data, idBytes(ebml.IDCluster))
	if off < 0 {
		return nil, 0, fmt.Errorf("corrupt: cluster not found")
	}
	out := append([]byte(nil), data...)
	sizeOff := off + 4
	if sizeOff+8 > len(out) {
		return nil, 0, fmt.Errorf("corrupt: cluster size out of range")
	}
	// Keep the 8-byte width marker (0x01) and claim 2^40 bytes in the seven
	// size bytes that follow it.
	out[sizeOff] = 0x01
	for i := sizeOff + 1; i < sizeOff+8; i++ {
		out[i] = 0
	}
	out[sizeOff+2] = 0x01 // 2^40
	return out, int64(off), nil
}

func idBytes(id uint64) []byte {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], uint32(id))
	switch {
	case id >= 1<<24:
		return b[:]
	case id >= 1<<16:
		return b[1:]
	case id >= 1<<8:
		return b[2:]
	}
	return b[3:]
}
