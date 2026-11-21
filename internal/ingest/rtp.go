package ingest

import (
	"io"

	"github.com/pion/rtp"

	"mediameld/internal/model"
)

// RTPPacket is a decoded transport packet with its receive-order offset and
// the still-32-bit timestamp. Calibration and unwrapping happen later in the
// timeline engine, because both depend on stream-wide history.
type RTPPacket struct {
	Seq       uint16
	Timestamp uint32
	Payload   []byte
	Offset    int64
}

// RTPResult is one demultiplexed RTP stream.
type RTPResult struct {
	Track   *model.Track
	Packets []RTPPacket
}

// DemuxRTP parses a length-prefixed RTP packet stream. The fixture frames every
// packet with a 4-byte big-endian length so packet boundaries survive a raw
// capture file. Malformed framing or header decoding is ERR_RTP_PACKET with
// the byte offset, never a silent skip.
func DemuxRTP(r io.ReadSeeker, trackID string) (RTPResult, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return RTPResult{}, err
	}
	var res RTPResult
	pos := 0
	for pos < len(data) {
		if pos+4 > len(data) {
			return res, model.NewError(model.CodeRTPPacket, "truncated length prefix").WithOffset(int64(pos))
		}
		n := int(beU32(data[pos:]))
		prefixAt := pos
		pos += 4
		if n < 12 || pos+n > len(data) {
			return res, model.NewError(model.CodeRTPPacket,
				"packet length %d invalid (remaining %d)", n, len(data)-pos).WithOffset(int64(prefixAt))
		}
		var pkt rtp.Packet
		if err := pkt.Unmarshal(data[pos : pos+n]); err != nil {
			return res, model.WrapError(model.CodeRTPPacket, err, "malformed RTP packet").WithOffset(int64(pos))
		}
		if pkt.Version != 2 {
			return res, model.NewError(model.CodeRTPPacket, "unsupported RTP version %d", pkt.Version).WithOffset(int64(pos))
		}
		res.Packets = append(res.Packets, RTPPacket{
			Seq: pkt.SequenceNumber, Timestamp: pkt.Timestamp,
			Payload: pkt.Payload, Offset: int64(pos),
		})
		pos += n
	}
	if len(res.Packets) == 0 {
		return res, model.NewError(model.CodeRTPPacket, "no RTP packets decoded")
	}
	res.Track = &model.Track{ID: trackID, Kind: model.KindVideo, Timescale: 90000, SampleDesc: "rtp-raw"}
	return res, nil
}
