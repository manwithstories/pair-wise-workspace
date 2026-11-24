package ingest

import (
	"fmt"

	"github.com/pion/rtp"

	"mediarun/internal/model"
	rtporder "mediarun/internal/rtp"
)

// Receive validates one RTP packet and folds it into the ordered stream.
// Malformed wire data is a corrupt-input error and never enters the buffer.
func (s *Stream) Receive(wire []byte) (Receipt, error) {
	if len(wire) < 12 {
		return Receipt{}, model.Classify(model.ClassCorrupt,
			fmt.Errorf("%w: rtp packet %d bytes", model.ErrTruncated, len(wire)))
	}
	var p rtp.Packet
	if err := p.Unmarshal(wire); err != nil {
		return Receipt{}, model.Classify(model.ClassCorrupt,
			fmt.Errorf("%w: rtp: %v", model.ErrBadLength, err))
	}
	if len(p.Payload) > MaxPayload {
		return Receipt{}, model.Classify(model.ClassCorrupt,
			fmt.Errorf("%w: payload %d", model.ErrLimit, len(p.Payload)))
	}
	return s.Push(rtporder.Packet{
		Sequence:  p.SequenceNumber,
		Timestamp: p.Timestamp,
		TrackID:   s.cfg.TrackID,
		Marker:    p.Marker,
		Payload:   append([]byte(nil), p.Payload...),
	})
}
