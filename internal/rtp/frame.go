package rtp

import (
	"errors"

	"github.com/bluenviron/gortsplib/v4/pkg/format/rtph264"
	"github.com/pion/rtp"
)

var annexBStartCode = []byte{0x00, 0x00, 0x00, 0x01}

// Framer turns one sealed frame's RTP packets into H.264 access-unit bytes
// in Annex-B form. It is the only place H.264 depacketization happens.
type Framer struct {
	dec         *rtph264.Decoder
	Frames      int // frames successfully produced
	FrameErrors int // sealed frames that failed H.264 depacketization
	Bytes       int // total frame bytes produced
}

// NewFramer returns a Framer ready for one track.
func NewFramer() (*Framer, error) {
	f := &Framer{dec: &rtph264.Decoder{}}
	if err := f.dec.Init(); err != nil {
		return nil, err
	}
	return f, nil
}

// Assemble depacketizes a sealed frame and counts it. A depacketization
// error is counted and reported as a nil frame, never propagated: one bad
// frame must not stall the media plane.
func (f *Framer) Assemble(pkts []*rtp.Packet) []byte {
	var out []byte
	for _, pkt := range pkts {
		nalus, err := f.dec.Decode(pkt)
		if err != nil {
			if errors.Is(err, rtph264.ErrMorePacketsNeeded) {
				continue // AU not complete yet, nothing to emit
			}
			f.FrameErrors++
			return nil
		}
		for _, nalu := range nalus {
			out = append(out, annexBStartCode...)
			out = append(out, nalu...)
		}
	}
	f.Frames++
	f.Bytes += len(out)
	return out
}
