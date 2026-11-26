// Package sample generates all local MP4/MXF/RTP fixtures deterministically.
// No repository-external media is read.
package sample

import (
	"encoding/binary"
	"fmt"
	"math/rand"
)

// SegmentCount is the fixed number of verified fragments in the demo.
const SegmentCount = 12

// PayloadSize keeps XOR groups simple with equal-length payloads.
const PayloadSize = 96

// Segments generates count deterministic media-segment payloads.
func Segments(seed int64, count int) [][]byte {
	if count <= 0 {
		count = SegmentCount
	}
	out := make([][]byte, count)
	for i := 0; i < count; i++ {
		out[i] = SegmentPayload(seed, i)
	}
	return out
}

// SegmentPayload returns one stable pseudo-random fragment. Equal seed/index
// always produce equal bytes, including across repeated demo invocations.
func SegmentPayload(seed int64, index int) []byte {
	r := rand.New(rand.NewSource(seed*1000003 + int64(index)*7919 + 17385))
	prefix := []byte(fmt.Sprintf("MEDIALAB segment seed=%d index=%02d ", seed, index))
	payload := make([]byte, PayloadSize)
	copy(payload, prefix)
	var tail [8]byte
	binary.BigEndian.PutUint64(tail[:], uint64(seed))
	copy(payload[PayloadSize-16:PayloadSize-8], tail[:])
	binary.BigEndian.PutUint64(tail[:], uint64(index))
	copy(payload[PayloadSize-8:], tail[:])
	_, _ = r.Read(payload[len(prefix) : PayloadSize-16])
	return payload
}

// BandwidthLadder parses and validates the comma-separated rung list.
func BandwidthLadder(raw []int) []int {
	if len(raw) == 0 {
		return []int{900, 280, 1200}
	}
	return append([]int(nil), raw...)
}
