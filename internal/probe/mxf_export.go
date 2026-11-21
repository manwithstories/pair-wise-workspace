package probe

import (
	"encoding/binary"
)

// This file exposes the small amount of the MXF profile the fixture writer
// needs. Keeping the catalogue private and exporting only lookups means the
// reader and the writer can never drift apart.

// PrimerEntry is one tag/UL pair for a primer pack.
type PrimerEntry struct {
	Tag uint16
	UL  UL
}

// PrimerCatalogue returns the profile's complete tag/UL list in a stable order.
func PrimerCatalogue() []PrimerEntry {
	out := make([]PrimerEntry, 0, len(mxfCatalog))
	for _, e := range mxfCatalog {
		out = append(out, PrimerEntry{Tag: e.Tag, UL: e.Prop.UL})
	}
	return out
}

// TagFor returns the wire tag for a property name.
func TagFor(name string) (uint16, bool) { return tagFor(name) }

// EncodeBER encodes a BER length.
func EncodeBER(v uint64) []byte { return encodeBER(v) }

// EncodeStrongRef serialises a strong reference.
func EncodeStrongRef(typeUL UL, umid [32]byte, trackID uint32) []byte {
	return encodeStrongRef(StrongRef{Type: typeUL, UMID: umid, TrackID: trackID})
}

// AppendUTF16 appends s as UTF-16BE.
func AppendUTF16(dst []byte, s string) []byte { return appendUTF16(dst, s) }

// EssenceKey returns the essence element key for a track number.
func EssenceKey(video bool, trackNo uint32) UL {
	if video {
		return essenceKey(keyPicture, trackNo)
	}
	return essenceKey(keySound, trackNo)
}

// EncodeUint32 is a convenience for fixture writers.
func EncodeUint32(v uint32) []byte {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	return b[:]
}

// EncodeUint64 is a convenience for fixture writers.
func EncodeUint64(v uint64) []byte {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], v)
	return b[:]
}

// EncodeRational encodes a numerator/denominator pair.
func EncodeRational(num, den int32) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint32(b, uint32(num))
	binary.BigEndian.PutUint32(b[4:], uint32(den))
	return b
}
