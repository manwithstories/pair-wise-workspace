package fixtgen

import (
	"bytes"
	"encoding/binary"
)

// applyFault mutates an otherwise valid BMFF byte slice. Mutations are
// deterministic and confined to the defect the segment declares.
func applyFault(out []byte, seg Segment, mdatStart int64) []byte {
	switch seg.Fault {
	case FaultTruncated:
		// Remove the last two samples' worth of bytes from the tail. The mdat
		// header still claims the original length, so the parser must detect
		// EOF and retain only complete samples.
		cut := 0
		if len(out) > 64 {
			cut = 40
		}
		return out[:len(out)-cut]

	case FaultBadLength:
		// Corrupt the first video track's stsz entry so it claims an
		// impossibly large sample. The parser sizes samples from stsz (not the
		// in-band AVCC prefix), must locate the fault by offset and quarantine
		// the track rather than allocate or panic.
		if pos := bytes.Index(out, []byte("stsz")); pos >= 0 {
			payload := pos + 4
			count := binary.BigEndian.Uint32(out[payload+8:])
			if count > 0 {
				binary.BigEndian.PutUint32(out[payload+12:], 0x7FFFFFFF)
			}
		}
		return out

	case FaultCorruptIndex:
		// stss already rewritten with a false sync; nothing else to mutate.
		return out

	default:
		return out
	}
}
