package pipeline

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"hash"

	"tsqc/internal/clock"
	"tsqc/internal/ts"
)

type semanticHash struct {
	h hash.Hash
}

func newSemanticHash() *semanticHash { return &semanticHash{h: sha256.New()} }

func (s *semanticHash) add(au *ts.AccessUnit) {
	var buf [29]byte
	binary.BigEndian.PutUint16(buf[0:], au.PID)
	buf[2] = byte(au.Kind)
	if au.Keyframe {
		buf[3] = 1
	}
	// Segment output is decoded from its first timestamp, so only values
	// modulo the 33-bit clock are reproducible across a reparse.
	binary.BigEndian.PutUint64(buf[4:], uint64(au.PTS%clock.Wrap))
	if au.HasDTS {
		buf[12] = 1
		binary.BigEndian.PutUint64(buf[13:], uint64(au.DTS%clock.Wrap))
	}
	binary.BigEndian.PutUint64(buf[21:], 0)
	sum := sha256.Sum256(au.Data)
	copy(buf[23:], sum[:6])
	s.h.Write(buf[:])
}

func (s *semanticHash) sum() string { return hex.EncodeToString(s.h.Sum(nil)) }

func semanticDigest(items []segItem) string {
	h := newSemanticHash()
	for i := range items {
		h.add(&items[i].au)
	}
	return h.sum()
}

// inputDigest canonicalises exactly what the segmenter consumed, including
// the unwrapped presentation timestamps. It is the cache input identity.
func inputDigest(items []segItem) string {
	h := sha256.New()
	var buf [20]byte
	for i := range items {
		au := &items[i].au
		binary.BigEndian.PutUint16(buf[0:], au.PID)
		buf[2] = byte(au.Kind)
		if au.Keyframe {
			buf[3] = 1
		}
		binary.BigEndian.PutUint64(buf[4:], uint64(au.PTS))
		binary.BigEndian.PutUint64(buf[12:], uint64(au.DTS))
		h.Write(buf[:])
		h.Write(au.Data)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// VerifySegment re-parses one segment and checks that the declared tracks,
// timestamps and packet order survive a round trip.
