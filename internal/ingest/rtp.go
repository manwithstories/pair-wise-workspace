package ingest

import (
	"io"
	"sort"

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

// RTPCalibration is the acquisition-clock view of a transport stream:
// 32-bit timestamps unwrapped in sequence order, then converted to unified
// nanoseconds. NS is indexed by source frame index.
type RTPCalibration struct {
	NS        []int64 // ns per frame index; -1 marks a frame that never arrived
	Order     []int   // frame indexes in receive order (duplicates retained)
	Monotonic bool    // NS is non-decreasing when read by frame index
}

// CalibrateRTP unwraps the 32-bit timestamps in sequence order rather than
// receive order, so a reordered packet still lands on the correct side of the
// wrap boundary. Frame zero becomes the zero reference.
func CalibrateRTP(res RTPResult, driftPPM int64) RTPCalibration {
	type pkt struct {
		seq uint16
		idx int
		ts  uint32
	}
	pkts := make([]pkt, 0, len(res.Packets))
	baseSeq := uint16(0xFFFF)
	for _, p := range res.Packets {
		if p.Seq < baseSeq {
			baseSeq = p.Seq
		}
	}
	for _, p := range res.Packets {
		pkts = append(pkts, pkt{seq: p.Seq, idx: int(p.Seq - baseSeq), ts: p.Timestamp})
	}
	// Receive order is preserved separately; it is what makes late detection
	// possible even though timestamps themselves are transport-ordered.
	out := RTPCalibration{Monotonic: true}
	for _, p := range pkts {
		out.Order = append(out.Order, p.idx)
	}
	sort.SliceStable(pkts, func(i, j int) bool { return pkts[i].seq < pkts[j].seq })
	var uniq []pkt
	seen := map[uint16]bool{}
	for _, p := range pkts {
		if seen[p.seq] {
			continue
		}
		seen[p.seq] = true
		uniq = append(uniq, p)
	}
	if len(uniq) == 0 {
		return out
	}
	raw := make([]uint32, len(uniq))
	maxIdx := 0
	for i, p := range uniq {
		raw[i] = p.ts
		if p.idx > maxIdx {
			maxIdx = p.idx
		}
	}
	ticks := model.Unwrap32(raw)
	clock := model.Clock{Timescale: 90000, DriftPPM: driftPPM}
	out.NS = make([]int64, maxIdx+1)
	for i := range out.NS {
		out.NS[i] = -1
	}
	for i, p := range uniq {
		out.NS[p.idx] = int64(clock.Calibrate(ticks[i] - ticks[0]))
	}
	prev := int64(-1)
	for _, ns := range out.NS {
		if ns < 0 {
			continue
		}
		if ns < prev {
			out.Monotonic = false
		}
		prev = ns
	}
	return out
}
