package ebml

import (
	"bytes"

	"mkvarchive/internal/model"
)

// elemBytes builds one known-size EBML element (test fixture helper).
func elemBytes(id uint32, payload []byte) []byte {
	var b bytes.Buffer
	idEnc, _ := EncodeID(id)
	sizeEnc, _ := EncodeVINT(uint64(len(payload)))
	b.Write(idEnc)
	b.Write(sizeEnc)
	b.Write(payload)
	return b.Bytes()
}

// elemUnknown builds an EBML element with the unknown-length VINT form.
func elemUnknown(id uint32, payload []byte, width int) []byte {
	var b bytes.Buffer
	idEnc, _ := EncodeID(id)
	sizeEnc, _ := EncodeVINTWidth(UnknownSizeValue(width), width)
	b.Write(idEnc)
	b.Write(sizeEnc)
	b.Write(payload)
	return b.Bytes()
}

// simpleBlock builds a Matroska SimpleBlock payload with track number,
// relative timecode, flags and verbatim frame bytes.
func simpleBlock(track uint64, relTC int16, flags byte, frame []byte) []byte {
	var b bytes.Buffer
	tn, _ := EncodeVINT(track - 1) // data value is track-1 for 1..127
	b.Write(tn)
	tc := make([]byte, 2)
	tc[0] = byte(uint16(relTC) >> 8)
	tc[1] = byte(uint16(relTC))
	b.Write(tc)
	b.WriteByte(flags)
	b.Write(frame)
	return b.Bytes()
}

// uintElem builds an unsigned-integer EBML element.
func uintElem(id uint32, v uint64) []byte {
	return elemBytes(id, EncodeUnsigned(v))
}

// minimalSegment returns an EBML header plus an unknown-length segment
// containing Info (with the given timestamp scale) and an empty Tracks.
func minimalSegment(timestampScale uint64) []byte {
	var b bytes.Buffer
	b.Write(elemBytes(model.IDEBMLHeader, []byte{0x42, 0x82, 0x88, 0x6D, 0x61, 0x74, 0x72, 0x6F, 0x73, 0x6B, 0x61}))
	info := uintElem(model.IDTimestampScale, timestampScale)
	tracks := elemBytes(model.IDTracks, nil)
	body := append(elemBytes(model.IDSegmentInfo, info), tracks...)
	b.Write(elemUnknown(model.IDSegment, body, 8))
	return b.Bytes()
}
