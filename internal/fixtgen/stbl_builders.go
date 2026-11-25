package fixtgen

import (
	"encoding/binary"
)

// This file holds the stbl leaf-box builders for the fixture BMFF writer. They
// are kept separate so each source file stays under the per-patch size limit.

func buildStts() box {
	body := make([]byte, 16)
	copy(body[0:4], full(0, 0))
	binary.BigEndian.PutUint32(body[4:8], 1)
	binary.BigEndian.PutUint32(body[8:12], SamplesPerTrack)
	binary.BigEndian.PutUint32(body[12:16], 1000)
	return *b("stts").raw(body)
}

func buildStsc() box {
	body := make([]byte, 20)
	copy(body[0:4], full(0, 0))
	binary.BigEndian.PutUint32(body[4:8], 1)
	binary.BigEndian.PutUint32(body[8:12], 1)
	binary.BigEndian.PutUint32(body[12:16], SamplesPerTrack)
	binary.BigEndian.PutUint32(body[16:20], 1)
	return *b("stsc").raw(body)
}

func buildCo64(offs []int64) box {
	body := make([]byte, 8+8*len(offs))
	copy(body[0:4], full(0, 0))
	binary.BigEndian.PutUint32(body[4:8], uint32(len(offs)))
	for i, o := range offs {
		binary.BigEndian.PutUint64(body[8+8*i:], uint64(o))
	}
	return *b("co64").raw(body)
}

func buildStsz(sizes []uint32) box {
	body := make([]byte, 12+4*len(sizes))
	copy(body[0:4], full(0, 0))
	binary.BigEndian.PutUint32(body[8:12], uint32(len(sizes)))
	for i, s := range sizes {
		binary.BigEndian.PutUint32(body[12+4*i:], s)
	}
	return *b("stsz").raw(body)
}

func u32Leaf(typ string, vals []uint32) *box {
	body := make([]byte, 8+4*len(vals))
	copy(body[0:4], full(0, 0))
	binary.BigEndian.PutUint32(body[4:8], uint32(len(vals)))
	for i, v := range vals {
		binary.BigEndian.PutUint32(body[8+4*i:], v)
	}
	return b(typ).raw(body)
}
