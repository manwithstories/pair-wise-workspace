package sample

import (
	"encoding/binary"
	"errors"
)

// ErrUnknownEdge is returned for an unrecognized edge fixture kind.
var ErrUnknownEdge = errors.New("unknown edge fixture kind")

// EdgeKind enumerates the auto-generated adverse fixtures used to prove
// isolation, recovery and rejection paths.
type EdgeKind string

const (
	EdgeTruncated    EdgeKind = "truncated"
	EdgeBadRTCP      EdgeKind = "bad-rtcp"
	EdgeTamperCache  EdgeKind = "tamper-cache"
	EdgeSlowConsumer EdgeKind = "slow-consumer"
)

// BuildEdgeFixture produces a framed datagram stream with one specific adverse
// condition injected. The returned bytes are fed through the same reader and
// pipeline as a normal fixture, exercising the corresponding handler.
//
//   - truncated:    the final datagram's length prefix promises more bytes
//                   than remain, forcing a Corrupt framing error.
//   - bad-rtcp:     a syntactically broken RTCP compound packet is inserted,
//                   which must be counted (BadRTCP) and skipped, not fatal.
func BuildEdgeFixture(kind EdgeKind, seed int64, seconds int) ([]byte, error) {
	r := DefaultRenditions()[0]
	stream, _, err := BuildRendition(r, ProgramDuration(seconds), seed, DamageConfig{})
	if err != nil {
		return nil, err
	}
	switch kind {
	case EdgeTruncated:
		// Append a length prefix advertising 200 bytes but supply only four.
		out := append([]byte(nil), stream...)
		var prefix [2]byte
		binary.BigEndian.PutUint16(prefix[:], 200)
		out = append(out, prefix[0], prefix[1], 0x80, 0x21, 0x00, 0x00)
		return out, nil

	case EdgeBadRTCP:
		// Insert an RTCP-range packet with an impossible block length between
		// two valid framed datagrams. It must be detected and skipped.
		bad := make([]byte, 8)
		bad[0] = 0x80
		bad[1] = 200 // SR type
		binary.BigEndian.PutUint16(bad[2:4], 999) // length runs past datagram
		bad[4], bad[5], bad[6], bad[7] = 0, 0, 0, 1
		framed := lengthPrefix(bad)
		// Splice after the first datagram's own prefix+body.
		if len(stream) < 2 {
			return framed, nil
		}
		firstLen := int(binary.BigEndian.Uint16(stream[:2]))
		spliceAt := 2 + firstLen
		if spliceAt > len(stream) {
			spliceAt = len(stream)
		}
		out := make([]byte, 0, len(stream)+len(framed))
		out = append(out, stream[:spliceAt]...)
		out = append(out, framed...)
		out = append(out, stream[spliceAt:]...)
		return out, nil

	case EdgeTamperCache, EdgeSlowConsumer:
		// These conditions are imposed at the pipeline/cache layer rather than
		// in the wire stream, so the clean fixture is returned unchanged.
		return stream, nil

	default:
		return nil, ErrUnknownEdge
	}
}
