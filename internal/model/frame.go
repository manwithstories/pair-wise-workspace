package model

import (
	"crypto/sha256"
	"encoding/binary"
)

// Color is the per-frame color metadata declared by the camera. Values are
// CICP-style code points (primaries, transfer characteristics, matrix
// coefficients) plus a video range flag.
type Color struct {
	Primaries uint8
	Transfer  uint8
	Matrix    uint8
	Range     uint8
}

// FrameMeta is the canonical, identity-bearing description of one frame. It is
// the input to both the metadata digest stored in the container and the cache
// provenance record, so a cached payload can be rejected when the declaring
// track, frame number, color description or timebase changes.
type FrameMeta struct {
	TrackID      uint32
	FrameNo      uint64
	CaptureUnits uint64
	Width        uint32
	Height       uint32
	CaptureUPS   uint32
	Timescale    uint32
	Color        Color
}

// canonicalMagic versions the byte layout below. Bumping it invalidates cache
// provenance from older builds instead of silently accepting it.
const canonicalMagic = "mediad/frame-meta/v1\x00"

// CanonicalBytes returns the fixed-layout encoding hashed into the metadata
// digest. A fixed length and explicit byte order make the digest identical on
// every host and independent of Go struct padding.
func (m FrameMeta) CanonicalBytes() []byte {
	b := make([]byte, 0, len(canonicalMagic)+4+8+8+4+4+4+4+4)
	b = append(b, canonicalMagic...)
	b = binary.BigEndian.AppendUint32(b, m.TrackID)
	b = binary.BigEndian.AppendUint64(b, m.FrameNo)
	b = binary.BigEndian.AppendUint64(b, m.CaptureUnits)
	b = binary.BigEndian.AppendUint32(b, m.Width)
	b = binary.BigEndian.AppendUint32(b, m.Height)
	b = binary.BigEndian.AppendUint32(b, m.CaptureUPS)
	b = binary.BigEndian.AppendUint32(b, m.Timescale)
	b = append(b, m.Color.Primaries, m.Color.Transfer, m.Color.Matrix, m.Color.Range)
	return b
}

// Digest is the SHA-256 of CanonicalBytes.
func (m FrameMeta) Digest() [32]byte { return sha256.Sum256(m.CanonicalBytes()) }

// DigestBytes is the SHA-256 of a frame payload or any other byte slice whose
// content identity is being declared.
func DigestBytes(p []byte) [32]byte { return sha256.Sum256(p) }

// DigestString is the SHA-256 of a string, used for manifests and index
// identifiers that must be comparable across runs.
func DigestString(s string) [32]byte { return sha256.Sum256([]byte(s)) }

// Frame represents one parsed frame record. Status says whether the frame may
// enter the index. A frame is never silently dropped: every non-OK status is
// surfaced in the report with its coordinate.
type Frame struct {
	Meta    FrameMeta
	Offset  int64 // byte offset of the frame box in the source container
	Size    uint32
	PTS     int64 // unified presentation timestamp, ticks
	Digest  [32]byte
	Status  FrameStatus
	Detail  string
	DeclPTS int64 // PTS as declared in the container (0 for v1 sources)
}

// FrameStatus classifies one frame record.
type FrameStatus uint8

const (
	// FrameOK means checksum, image and sequence checks all passed.
	FrameOK FrameStatus = iota
	// FrameHashMismatch means the payload does not match its declared SHA-256.
	FrameHashMismatch
	// FrameImageInvalid means the payload is not a decodable image of the
	// declared size.
	FrameImageInvalid
	// FrameTruncatedTail means the source ended inside this frame's payload.
	FrameTruncatedTail
	// FrameDuplicate means the frame number repeats with identical content and
	// was folded into the earlier copy.
	FrameDuplicate
	// FrameConflict means the frame number repeats or rewinds with different
	// content; the frame is isolated and reported.
	FrameConflict
)

func (s FrameStatus) String() string {
	switch s {
	case FrameHashMismatch:
		return "hash_mismatch"
	case FrameImageInvalid:
		return "image_invalid"
	case FrameTruncatedTail:
		return "truncated_tail"
	case FrameDuplicate:
		return "duplicate"
	case FrameConflict:
		return "conflict"
	default:
		return "ok"
	}
}

// Usable reports whether the frame may be indexed and rendered.
func (s FrameStatus) Usable() bool { return s == FrameOK }
