// Package frame implements the versioned frame-sequence adapter (v1).
// Delivery format: each frame is a binary record carried as the payload
// of one RTP packet (pion/rtp), giving a 32-bit capture clock and packet
// boundaries. The adapter validates sequence numbers, timestamps and
// SHA-256 checksums and converts frames onto the unified time base.
package frame

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math/rand"

	"media-check/internal/model"
)

const (
	AdapterID = "frame-seq"
	Version   = "v1"
)

// Adapter is the frame-sequence adapter v1.
type Adapter struct{}

func (Adapter) ID() string      { return AdapterID }
func (Adapter) Version() string { return Version }

// Record is one delivered frame with its declared metadata and checksum.
type Record struct {
	Seq       uint32
	CaptureTS uint32
	Keyframe  bool
	Width     uint16
	Height    uint16
	Color     model.ColorMeta
	Payload   []byte
	Sum       [32]byte // SHA-256 of Payload, declared by the supplier
}

// GenOptions parameterises deterministic sample generation.
type GenOptions struct {
	Count       int
	StartSeq    uint32
	StartTS     uint32
	TSDelta     uint32
	GOPSize     int
	Width       uint16
	Height      uint16
	PayloadSize int
	Seed        int64
	Color       model.ColorMeta
}

// Generate builds a deterministic, well-formed frame sequence.
func Generate(o GenOptions) []Record {
	rng := rand.New(rand.NewSource(o.Seed))
	recs := make([]Record, 0, o.Count)
	for i := 0; i < o.Count; i++ {
		payload := make([]byte, o.PayloadSize)
		rng.Read(payload)
		// embed the sequence number so payloads differ predictably
		binary.BigEndian.PutUint32(payload[:4], o.StartSeq+uint32(i))
		recs = append(recs, Record{
			Seq:       o.StartSeq + uint32(i),
			CaptureTS: o.StartTS + uint32(i)*o.TSDelta,
			Keyframe:  o.GOPSize > 0 && i%o.GOPSize == 0,
			Width:     o.Width,
			Height:    o.Height,
			Color:     o.Color,
			Payload:   payload,
			Sum:       sha256.Sum256(payload),
		})
	}
	return recs
}

// recordHeaderLen is seq|ts|flags|width|height|colorLen = 4+4+1+2+2+2.
const recordHeaderLen = 15

// Marshal serialises a record deterministically (header, colour tags,
// payload, declared checksum).
func (r Record) Marshal() []byte {
	color := r.Color.Primaries + "|" + r.Color.Transfer + "|" + r.Color.Matrix
	buf := make([]byte, 0, recordHeaderLen+len(color)+len(r.Payload)+sha256.Size)
	var hdr [recordHeaderLen]byte
	binary.BigEndian.PutUint32(hdr[0:4], r.Seq)
	binary.BigEndian.PutUint32(hdr[4:8], r.CaptureTS)
	if r.Keyframe {
		hdr[8] = 1
	}
	binary.BigEndian.PutUint16(hdr[9:11], r.Width)
	binary.BigEndian.PutUint16(hdr[11:13], r.Height)
	binary.BigEndian.PutUint16(hdr[13:15], uint16(len(color)))
	buf = append(buf, hdr[:]...)
	buf = append(buf, color...)
	buf = append(buf, r.Payload...)
	buf = append(buf, r.Sum[:]...)
	return buf
}

// ParseRecord decodes one record, validating all lengths before reading.
func ParseRecord(b []byte) (Record, error) {
	var r Record
	if len(b) < recordHeaderLen+sha256.Size {
		return r, fmt.Errorf("record too short: %d bytes", len(b))
	}
	r.Seq = binary.BigEndian.Uint32(b[0:4])
	r.CaptureTS = binary.BigEndian.Uint32(b[4:8])
	r.Keyframe = b[8] != 0
	r.Width = binary.BigEndian.Uint16(b[9:11])
	r.Height = binary.BigEndian.Uint16(b[11:13])
	clen := int(binary.BigEndian.Uint16(b[13:15]))
	if len(b) < recordHeaderLen+clen+sha256.Size {
		return r, fmt.Errorf("record colour field overruns buffer: %d", clen)
	}
	color := string(b[recordHeaderLen : recordHeaderLen+clen])
	parts := split3(color)
	r.Color = model.ColorMeta{Primaries: parts[0], Transfer: parts[1], Matrix: parts[2]}
	payloadEnd := len(b) - sha256.Size
	r.Payload = append([]byte(nil), b[recordHeaderLen+clen:payloadEnd]...)
	copy(r.Sum[:], b[payloadEnd:])
	return r, nil
}

func split3(s string) [3]string {
	var out [3]string
	start, idx := 0, 0
	for i := 0; i < len(s) && idx < 2; i++ {
		if s[i] == '|' {
			out[idx] = s[start:i]
			idx++
			start = i + 1
		}
	}
	out[idx] = s[start:]
	return out
}
