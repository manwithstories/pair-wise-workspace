package ebml

import "bytes"

// EncodeElement builds one known-size EBML element from its id and
// payload. It is shared by production writers and in-package fixtures.
func EncodeElement(id uint32, payload []byte) []byte {
	var b bytes.Buffer
	idEnc, _ := EncodeID(id)
	sizeEnc, _ := EncodeVINT(uint64(len(payload)))
	b.Write(idEnc)
	b.Write(sizeEnc)
	b.Write(payload)
	return b.Bytes()
}

// EncodeElementUnknown builds an unknown-length EBML element of the given
// VINT width carrying payload verbatim.
func EncodeElementUnknown(id uint32, payload []byte, width int) []byte {
	var b bytes.Buffer
	idEnc, _ := EncodeID(id)
	sizeEnc, _ := EncodeVINTWidth(UnknownSizeValue(width), width)
	b.Write(idEnc)
	b.Write(sizeEnc)
	b.Write(payload)
	return b.Bytes()
}
