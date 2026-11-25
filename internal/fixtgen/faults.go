package fixtgen

import (
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
		// Corrupt the first video sample's length prefix inside mdat so it
		// claims an impossibly large size. The parser must locate the fault by
		// offset and quarantine, rather than panic or allocate.
		if int(mdatStart)+8+4 < len(out) {
			// mdat header is 8 bytes; first 4 bytes are the AVCC length.
			pos := int(mdatStart) + 8
			binary.BigEndian.PutUint32(out[pos:], 0x7FFFFFFF)
		}
		return out

	case FaultCorruptIndex:
		// stss already rewritten with a false sync; nothing else to mutate.
		return out

	default:
		return out
	}
}
