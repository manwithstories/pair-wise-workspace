package fixture

import (
	"errors"

	"review/internal/codec"
)

// Corruption kinds; they map onto the three exit-code classes upstream.
const (
	CorruptTruncateTail   = "truncate-tail"
	CorruptTruncateUnknown = "truncate-unknown"
	CorruptOutOfRange     = "out-of-range-block"
	CorruptRTPDuplicate   = "rtp-duplicate-seq"
)

// TruncateTail cuts a valid document so the final byte is missing inside a
// sized frame; the parser must name the first missing offset rather than
// read past it.
func TruncateTail(d Document) []byte {
	full := d.Build()
	return full[:len(full)-1]
}

// TruncateUnknown builds a document whose final cluster uses an unknown
// size and whose physical bytes are cut mid-payload. Because the cluster
// frame is bounded by an unknown-size segment ending at EOF, a short read
// here is a clean EOF; the sized block inside still exposes the truncation.
func TruncateUnknown(seed int64) []byte {
	d := RemoteNarration(seed)
	d.UnknownSegment = true
	d.UnknownCluster = true
	full := d.Build()
	// Keep the headers intact; cut the last payload region.
	cut := len(full) - 7
	if cut < 16 {
		cut = 16
	}
	return full[:cut]
}

// OutOfRangeBlock mutates a valid document so one simple block's size VINT
// claims more bytes than the enclosing sized cluster contains. The size
// VINT width is preserved so all later offsets stay valid and the reader
// trips its boundary check rather than a generic truncation.
func OutOfRangeBlock(d Document) []byte {
	d.UnknownSegment = false
	d.UnknownCluster = false
	full := d.Build()
	idBytes, err := codec.EncodeID(idSimpleBlock)
	if err != nil {
		panic(err)
	}
	idx := findPattern(full, idBytes)
	if idx < 0 {
		panic("fixture: no simple block to corrupt")
	}
	sizePos := idx + len(idBytes)
	width := vintWidth(full[sizePos])
	if width <= 0 || sizePos+width > len(full) {
		panic("fixture: cannot read block size VINT")
	}
	out := append([]byte(nil), full...)
	// Largest *known* value encodable in this VINT width (all data bits
	// one except the final bit), guaranteed to overrun the cluster frame
	// while keeping every later byte offset unchanged.
	out[sizePos] = markerLead(width)
	for i := 1; i < width; i++ {
		out[sizePos+i] = 0xFF
	}
	out[sizePos+width-1] = 0xFE
	return out
}

// vintWidth returns the VINT width declared by a lead byte, or 0 if illegal.
func vintWidth(lead byte) int {
	if lead == 0 {
		return 0
	}
	width := 1
	for mask := byte(0x80); lead&mask == 0; mask >>= 1 {
		width++
	}
	return width
}

// markerLead returns the lead byte whose only set bit is the VINT marker.
func markerLead(width int) byte {
	return 0x80 >> uint(width-1)
}

// RTPDuplicate builds a synthetic RTP packet capture with one duplicated
// sequence number, used to exercise de-duplication in the timing sampler.
// Each entry is a marshalled RTP packet (see CaptureRTP).
func RTPDuplicate(seed int64) [][]byte {
	base := CaptureRTP(seed, 24)
	if len(base) < 8 {
		return base
	}
	out := make([][]byte, 0, len(base)+1)
	out = append(out, base[:5]...)
	out = append(out, base[4]) // exact duplicate sequence number
	out = append(out, base[5:]...)
	return out
}

// RecoveryArtifact is the on-disk state left behind by a simulated
// interrupted run: a completed prefix plus a stale temp file that never
// reached atomic commit.
type RecoveryArtifact struct {
	Committed [][]byte // fragment outputs already renamed into place
	TempName  string   // leftover *.tmp path fragment name
	TempData  []byte   // partial bytes that must be ignored/replaced
}

// InterruptedRun simulates a crash after three committed fragments and
// one partial temp write. Re-running must leave identical segment hashes.
func InterruptedRun(seed int64) RecoveryArtifact {
	d := StudioInterview(seed)
	stream := d.Build()
	step := len(stream) / 4
	if step == 0 {
		step = 1
	}
	committed := [][]byte{
		hashTag(1, stream[:step]),
		hashTag(2, stream[step:step*2]),
		hashTag(3, stream[step*2:step*3]),
	}
	return RecoveryArtifact{
		Committed: committed,
		TempName:  "fragment-004.tmp",
		TempData:  stream[step*3 : step*3+1],
	}
}

// findPattern returns the first index of pat in b, or -1.
func findPattern(b, pat []byte) int {
	for i := 0; i+len(pat) <= len(b); i++ {
		match := true
		for j := range pat {
			if b[i+j] != pat[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

// errFixtureCorrupt marks intentionally invalid builder input.
var errFixtureCorrupt = errors.New("fixture: corruption construction failed")

// hashTag wraps fragment bytes with a deterministic one-byte tag so callers
// can verify ordering without depending on a particular hash algorithm.
func hashTag(n int, b []byte) []byte {
	out := make([]byte, 0, len(b)+1)
	out = append(out, byte(n))
	return append(out, b...)
}
