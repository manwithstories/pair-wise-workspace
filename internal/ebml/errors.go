package ebml

import "fmt"

// MaxElementSize caps a single declared element payload at 64 MiB so a
// corrupt length field cannot drive unbounded memory allocation.
const MaxElementSize = 64 << 20

// Element IDs needed across the pipeline. Full Matroska semantics live in
// the fixture and timeline packages; the reader itself is schema-agnostic.
const (
	IDEBML           = 0x1A45DFA3
	IDSegment        = 0x18538067
	IDInfo           = 0x1549A966
	IDTracks         = 0x1654AE6B
	IDTrackEntry     = 0xAE
	IDTrackNumber    = 0xD7
	IDTrackUID       = 0x73C5
	IDTrackType      = 0x83
	IDCodecID        = 0x86
	IDDefaultDur     = 0x23E383
	IDAudio          = 0xE1
	IDChannels       = 0x9F
	IDSamplingFreq   = 0xB5
	IDBitDepth       = 0x6264
	IDColour         = 0x55B0
	IDMatrixCoeffs   = 0x55B1
	IDTransferChar   = 0x55BA
	IDPrimaries      = 0x55BB
	IDCluster        = 0x1F43B675
	IDTimecode       = 0xE7
	IDSimpleBlock    = 0xA3
	IDBlockGroup     = 0xA0
	IDBlock          = 0xA1
	IDTimestampScale = 0x2AD7B1
	IDDuration       = 0x4489
)

// reservedIDMarker bounds legal element-id values.
const reservedIDMarker = 0xFFFFFFFF

// TruncatedError reports that the stream ended inside an element header or
// payload. MissingOffset is the first absolute byte offset that could not
// be satisfied; callers must stop parsing rather than read past it.
type TruncatedError struct {
	MissingOffset int64
	ElementID     uint32
	What          string
}

func (e *TruncatedError) Error() string {
	return fmt.Sprintf("ebml: truncated stream at offset %d (%s, element 0x%X)",
		e.MissingOffset, e.What, e.ElementID)
}

// LimitError reports a declared element size exceeding MaxElementSize.
type LimitError struct {
	ElementID uint32
	Size      uint64
	Offset    int64
}

func (e *LimitError) Error() string {
	return fmt.Sprintf("ebml: element 0x%X size %d exceeds limit at offset %d",
		e.ElementID, e.Size, e.Offset)
}

// BoundaryError reports a payload read that would cross the enclosing
// master-element boundary (e.g. an out-of-range block).
type BoundaryError struct {
	ElementID uint32
	Offset    int64
	Want      int64
	FrameEnd  int64
}

func (e *BoundaryError) Error() string {
	return fmt.Sprintf("ebml: element 0x%X read at %d wants %d bytes beyond frame end %d",
		e.ElementID, e.Offset, e.Want, e.FrameEnd)
}
