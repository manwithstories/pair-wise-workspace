package parse

import (
	"encoding/binary"
	"fmt"
	"os"

	"avscan/internal/model"
)

// ReadRTP1File reads a synthetic "RTP1" framed capture into packet records.
// Bounds are checked on every frame so a truncated or corrupt header cannot
// drive an out-of-range slice.
func ReadRTP1File(path, streamName string, clockRate uint32) ([]RTPPacketRecord, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	if len(raw) < 4 || string(raw[:4]) != "RTP1" {
		return nil, fmt.Errorf("%w: missing RTP1 magic", model.ErrDamagedInput)
	}
	pos := 4
	var recs []RTPPacketRecord
	for pos < len(raw) {
		if pos+4 > len(raw) {
			return recs, fmt.Errorf("%w: truncated frame header at %d",
				model.ErrDamagedInput, pos)
		}
		frameLen := int(binary.BigEndian.Uint32(raw[pos : pos+4]))
		pos += 4
		if frameLen <= 0 || pos+frameLen > len(raw) {
			return recs, fmt.Errorf("%w: frame length %d out of range at %d",
				model.ErrDamagedInput, frameLen, pos)
		}
		if len(recs) >= maxRTPPackets {
			return recs, fmt.Errorf("%w: packet count exceeds %d",
				model.ErrLimitExceeded, maxRTPPackets)
		}
		pkt := make([]byte, frameLen)
		copy(pkt, raw[pos:pos+frameLen])
		recs = append(recs, RTPPacketRecord{
			StreamName: streamName, ClockRate: clockRate, Raw: pkt,
		})
		pos += frameLen
	}
	return recs, nil
}
