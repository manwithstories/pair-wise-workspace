package ts

import (
	"context"
	"fmt"
	"io"
	"sync/atomic"

	"github.com/asticode/go-astits"
)

// Writer delegates TS packet serialization to go-astits. This keeps the
// dependency constrained to packet-level reading/writing.
type Writer struct {
	m       *astits.Muxer
	w       io.Writer
	written int64
}

// NewWriter creates a 188-byte packet writer.
func NewWriter(w io.Writer) *Writer {
	return &Writer{m: astits.NewMuxer(context.Background(), w), w: w}
}

// Write emits one packet. adaptation may be nil. Continuity is supplied by the
// caller so that rewritten PSI and original ES packets remain deterministic.
func (w *Writer) Write(pid uint16, cc uint8, pusi bool, payload []byte, adaptation *Adaptation) error {
	if len(payload) > 184 {
		return fmt.Errorf("ts: payload too large: %d", len(payload))
	}
	p := &astits.Packet{Header: &astits.PacketHeader{
		PID:                       pid,
		HasPayload:                true,
		PayloadUnitStartIndicator: pusi,
		ContinuityCounter:         cc,
	}}
	if adaptation != nil {
		p.Header.HasAdaptationField = true
		p.AdaptationField = &astits.PacketAdaptationField{
			DiscontinuityIndicator: adaptation.DiscontinuityIndicator,
			RandomAccessIndicator:  adaptation.RandomAccessIndicator,
			HasPCR:                 adaptation.HasPCR,
		}
		if adaptation.HasPCR {
			p.AdaptationField.PCR = &astits.ClockReference{Base: adaptation.PCRBase, Extension: adaptation.PCRExtension}
		}
	}
	p.Payload = payload
	n, err := w.m.WritePacket(p)
	if err != nil {
		return fmt.Errorf("ts: writing packet: %w", err)
	}
	atomic.AddInt64(&w.written, int64(n))
	return nil
}

// WritePacket preserves the source packet's PID, CC, payload and PCR while
// normalizing it to 188 bytes.
func (w *Writer) WritePacket(p *Packet) error {
	var adaptation *Adaptation
	if p.Header.AdaptationField {
		a := p.Adapt
		adaptation = &a
	}
	return w.Write(p.Header.PID, p.Header.ContinuityCounter, p.Header.PayloadUnitStart, p.Payload, adaptation)
}

// Written reports the number of bytes emitted.
func (w *Writer) Written() int64 { return atomic.LoadInt64(&w.written) }
