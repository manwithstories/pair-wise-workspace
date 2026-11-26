package transport

import (
	"crypto/sha256"
	"encoding/hex"

	"mediad/internal/model"
)

// Packet is one addressed transport fragment. Checksum is the lowercase
// SHA-256 of Data.
type Packet struct {
	StreamID       string `json:"stream_id"`
	Seq            uint64 `json:"seq"`
	Offset         int64  `json:"offset"`
	Data           []byte `json:"-"`
	Checksum       string `json:"checksum"`
	Retransmission bool   `json:"retransmission,omitempty"`
}

func (p *Packet) ValidateChecksum() error {
	sum := sha256.Sum256(p.Data)
	want := hex.EncodeToString(sum[:])
	if p.Checksum == "" {
		p.Checksum = want
	}
	if p.Checksum != want {
		return model.New(model.CodeIntegrity, model.StageTransport, "packet checksum mismatch").
			WithOffset(p.Offset)
	}
	return nil
}

func NewPacket(stream string, seq uint64, offset int64, data []byte) Packet {
	sum := sha256.Sum256(data)
	return Packet{
		StreamID: stream,
		Seq:      seq,
		Offset:   offset,
		Data:     append([]byte(nil), data...),
		Checksum: hex.EncodeToString(sum[:]),
	}
}

type Fingerprint struct {
	StreamID string
	Seq      uint64
	Offset   int64
	Checksum string
}

func (p Packet) Fingerprint() Fingerprint {
	return Fingerprint{StreamID: p.StreamID, Seq: p.Seq, Offset: p.Offset, Checksum: p.Checksum}
}
