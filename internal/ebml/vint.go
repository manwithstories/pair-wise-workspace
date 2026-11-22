package ebml

import "fmt"

// ErrInvalidVINT reports a variable-length integer whose first byte has no
// terminator within the permitted width.
var ErrInvalidVINT = fmt.Errorf("ebml: invalid vint")

// ID is a Matroska element id.  Ids keep their marker bit, so the EBML header
// is 0x1A45DFA3 and Cluster is 0x1F43B675.
type ID uint32

// Well-known element ids used by the archive pipeline.
const (
	IDEBML                ID = 0x1A45DFA3
	IDEBMLVersion         ID = 0x4286
	IDEBMLReadVersion     ID = 0x42F7
	IDEBMLMaxIDLength     ID = 0x42F2
	IDEBMLMaxSizeLength   ID = 0x42F3
	IDDocType             ID = 0x4282
	IDDocTypeVersion      ID = 0x4287
	IDSegment             ID = 0x18538067
	IDSeekHead            ID = 0x114D9B74
	IDInfo                ID = 0x1549A966
	IDTimestampScale      ID = 0x2AD7B1
	IDDuration            ID = 0x4489
	IDMuxingApp           ID = 0x4D80
	IDWritingApp          ID = 0x5741
	IDTracks              ID = 0x1654AE6B
	IDTrackEntry          ID = 0xAE
	IDTrackNumber         ID = 0xD7
	IDTrackUID            ID = 0x73C5
	IDTrackType           ID = 0x83
	IDFlagEnabled         ID = 0xB9
	IDFlagDefault         ID = 0x88
	IDCodecID             ID = 0x86
	IDCodecPrivate        ID = 0x63A2
	IDDefaultDuration     ID = 0x23E383
	IDTrackTimestampScale ID = 0x23314F
	IDLanguage            ID = 0x22B59C
	IDCodecName           ID = 0x258688
	IDVideo               ID = 0xE0
	IDPixelWidth          ID = 0xB0
	IDPixelHeight         ID = 0xBA
	IDAudio               ID = 0xE1
	IDSamplingFrequency   ID = 0xB5
	IDChannels            ID = 0x9F
	IDCluster             ID = 0x1F43B675
	IDClusterTimestamp    ID = 0xE7
	IDSimpleBlock         ID = 0xA3
	IDBlockGroup          ID = 0xA0
	IDBlock               ID = 0xA1
	IDBlockDuration       ID = 0x9B
	IDReferenceBlock      ID = 0xFB
	IDPosition            ID = 0xA7
	IDPrevSize            ID = 0xAB
	IDCues                ID = 0x1C53BB6B
	IDTags                ID = 0x1254C367
	IDAttachments         ID = 0x1941A469
	IDChapters            ID = 0x1043A770
	IDVoid                ID = 0xEC
	IDCRC32               ID = 0xBF
	IDSilentTracks        ID = 0x5854
	IDEncryptedBlock      ID = 0xAF
	IDCuePoint            ID = 0xBB
	IDCueTime             ID = 0xB3
	IDCueTrackPositions   ID = 0xB7
	IDCueTrack            ID = 0xF7
	IDCueClusterPosition  ID = 0xF1
	IDCueRelativePosition ID = 0xF0
	IDTracksTrick         ID = 0x6D80
)

// SizeInfo describes a parsed element size field.
type SizeInfo struct {
	// Value is the payload size in bytes.  Meaningless when Unknown.
	Value uint64
	// Length is the encoded width of the size field in bytes (1..8).
	Length int
	// Unknown is set when every value bit is one; the element then extends
	// until a sibling or end of the enclosing master element.
	Unknown bool
}

// maxIDLength and maxSizeLength match Matroska's documented maxima; the
// EBML header can advertise smaller values and the reader enforces those.
const (
	maxIDLength   = 4
	maxSizeLength = 8
)

// vintLength returns the encoded width implied by the first byte.
func vintLength(first byte, max int) (int, error) {
	if first == 0 {
		return 0, ErrInvalidVINT
	}
	n := 1
	for mask := byte(0x80); mask != 0 && first&mask == 0; mask >>= 1 {
		n++
	}
	if n > max {
		return 0, fmt.Errorf("%w: width %d exceeds max %d", ErrInvalidVINT, n, max)
	}
	return n, nil
}

// ParseID decodes an element id from buf.  Ids retain marker bits.
func ParseID(buf []byte, maxLen int) (ID, int, error) {
	if len(buf) == 0 {
		return 0, 0, ErrInvalidVINT
	}
	if maxLen <= 0 || maxLen > maxIDLength {
		maxLen = maxIDLength
	}
	n, err := vintLength(buf[0], maxLen)
	if err != nil {
		return 0, 0, err
	}
	if len(buf) < n {
		return 0, 0, fmt.Errorf("ebml: truncated id: want %d bytes, have %d", n, len(buf))
	}
	var v uint64
	for i := 0; i < n; i++ {
		v = v<<8 | uint64(buf[i])
	}
	return ID(v), n, nil
}

// ParseSize decodes an element size from buf.  The marker bit is stripped and
// an all-ones value is reported as Unknown.
func ParseSize(buf []byte, maxLen int) (SizeInfo, error) {
	if len(buf) == 0 {
		return SizeInfo{}, ErrInvalidVINT
	}
	if maxLen <= 0 || maxLen > maxSizeLength {
		maxLen = maxSizeLength
	}
	n, err := vintLength(buf[0], maxLen)
	if err != nil {
		return SizeInfo{}, err
	}
	if len(buf) < n {
		return SizeInfo{}, fmt.Errorf("ebml: truncated size: want %d bytes, have %d", n, len(buf))
	}
	v := uint64(buf[0]) & (0xFF >> uint(n))
	allOnes := v == (0xFF >> uint(n))
	for i := 1; i < n; i++ {
		v = v<<8 | uint64(buf[i])
		if buf[i] != 0xFF {
			allOnes = false
		}
	}
	return SizeInfo{Value: v, Length: n, Unknown: allOnes}, nil
}

// EncodeSize writes a size as a minimal-width VINT.  Unknown sizes are
// encoded at the requested width.
func EncodeSize(v uint64, width int) []byte {
	if width < 1 || width > maxSizeLength {
		width = 1
		for width < maxSizeLength && v > (1<<(7*uint(width)))-2 {
			width++
		}
	}
	out := make([]byte, width)
	x := v
	for i := width - 1; i >= 0; i-- {
		out[i] = byte(x)
		x >>= 8
	}
	out[0] |= 0x80 >> uint(width-1)
	return out
}

// EncodeUnknownSize returns the all-ones size encoding at the given width.
func EncodeUnknownSize(width int) []byte {
	if width < 1 || width > maxSizeLength {
		width = 8
	}
	out := make([]byte, width)
	for i := range out {
		out[i] = 0xFF
	}
	// Width n uses the marker bit 1<<(8-n); value bits all ones means the
	// first byte is 0xFF>>(n-1), e.g. 0xFF, 0x7F, 0x3F, ... 0x01.
	out[0] = 0xFF >> uint(width-1)
	return out
}

// parseSignedVINT decodes an EBML-lacing signed vint.
func parseSignedVINT(buf []byte) (int64, int, error) {
	if len(buf) == 0 {
		return 0, 0, ErrInvalidVINT
	}
	n, err := vintLength(buf[0], 8)
	if err != nil {
		return 0, 0, err
	}
	if len(buf) < n {
		return 0, 0, fmt.Errorf("ebml: truncated signed vint")
	}
	v := int64(buf[0]) & int64(0xFF>>uint(n))
	for i := 1; i < n; i++ {
		v = v<<8 | int64(buf[i])
	}
	bias := int64(1)<<(7*uint(n)-1) - 1
	return v - bias, n, nil
}
