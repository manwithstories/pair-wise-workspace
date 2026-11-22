package fixture

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"sort"

	"mediad/internal/fault"
)

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// sortedFaults returns faults ordered deterministically for reporting.
func sortedFaults(fs []fault.Fault) []fault.Fault {
	out := append([]fault.Fault(nil), fs...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Loc.Segment != out[j].Loc.Segment {
			return out[i].Loc.Segment < out[j].Loc.Segment
		}
		return out[i].Loc.Offset < out[j].Loc.Offset
	})
	return out
}

func box(typ string, payload []byte) []byte {
	out := make([]byte, 8, 8+len(payload))
	binary.BigEndian.PutUint32(out[:4], uint32(8+len(payload)))
	copy(out[4:], typ)
	return append(out, payload...)
}

func be16(v uint16) []byte { b := make([]byte, 2); binary.BigEndian.PutUint16(b, v); return b }
func be32(v uint32) []byte { b := make([]byte, 4); binary.BigEndian.PutUint32(b, v); return b }
func be64(v uint64) []byte { b := make([]byte, 8); binary.BigEndian.PutUint64(b, v); return b }

func concat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}
