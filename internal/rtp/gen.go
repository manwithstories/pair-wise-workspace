package rtp

import (
	"encoding/binary"
	"math/rand"

	"medpipe/internal/fault"

	"github.com/pion/rtp"
)

// Generator defaults and hard limits.
const (
	// ClockRate is the RTP clock rate used by generated tracks.
	ClockRate = 48000
	// TicksPerPacket is the RTP timestamp step per packet (20 ms).
	TicksPerPacket = 960
	// MaxTrackSeconds caps a single track's duration.
	MaxTrackSeconds = 6 * 3600
	// MaxPayloadSize caps one packet's payload.
	MaxPayloadSize = 1500
	// SRInterval is how often (in sent packets) a sender report is emitted.
	SRInterval = 256
)

// GenOptions controls deterministic sample-stream generation.
type GenOptions struct {
	Seed             int64
	Tracks           int
	PacketsPerTrack  int
	PayloadSize      int     // bytes per RTP payload
	LossRate         float64 // fraction of RTP packets dropped in transit
	DupRate          float64 // fraction of RTP packets duplicated
	CorruptRate      float64 // fraction of RTP packets with a broken header
	ReorderWindow    int     // shuffle window applied to the record stream
	ClockJumpTrack   int     // track whose RTCP clock jumps mid-stream; -1 disables
	ClockJumpSeconds int64   // size of the injected jump
	TailLoss         int     // highest-indexed packets of each track to drop
	TruncateTail     int     // dangling partial-header bytes appended at EOF (0..6)
	SeqNearWrap      bool    // start sequence numbers near the 16-bit wrap
}

// Generated holds a generated stream plus ground truth for tests.
type Generated struct {
	Stream       []byte
	SentPerTrack []uint32 // packets sent by each source, before loss
	RTPRecords   int      // RTP records present in the stream (post-loss, incl. dups)
}

// Validate checks generator options against hard limits.
func (o *GenOptions) Validate() error {
	if o.Tracks < 1 || o.Tracks > MaxTracks {
		return fault.New(fault.KindResource, int64(o.Tracks), fault.Unknown, 0, false,
			"track count out of range")
	}
	if o.PacketsPerTrack < 1 {
		return fault.New(fault.KindResource, fault.Unknown, fault.Unknown, 0, false,
			"empty input: packets per track must be positive")
	}
	dur := int64(o.PacketsPerTrack) * TicksPerPacket / ClockRate
	if dur > MaxTrackSeconds {
		return fault.New(fault.KindResource, fault.Unknown, fault.Unknown, 0, false,
			"track duration exceeds limit")
	}
	if o.PayloadSize < 1 || o.PayloadSize > MaxPayloadSize {
		return fault.New(fault.KindResource, fault.Unknown, fault.Unknown, 0, false,
			"payload size out of range")
	}
	if o.ReorderWindow < 0 || o.ReorderWindow > 4096 {
		return fault.New(fault.KindResource, fault.Unknown, fault.Unknown, 0, false,
			"reorder window out of range")
	}
	if o.TruncateTail < 0 || o.TruncateTail >= HeaderLen {
		return fault.New(fault.KindResource, fault.Unknown, fault.Unknown, 0, false,
			"truncate tail out of range")
	}
	return nil
}

type genItem struct {
	isSR bool
	pkt  []byte
	sr   *SenderReport
	idx  int // original send index within the track
}

// Generate builds a deterministic framed RTP/RTCP stream.
func Generate(opts GenOptions) (*Generated, error) {
	if err := opts.Validate(); err != nil {
		return nil, err
	}
	rng := rand.New(rand.NewSource(opts.Seed))
	g := &Generated{SentPerTrack: make([]uint32, opts.Tracks)}
	perTrack := make([][]genItem, opts.Tracks)
	for t := 0; t < opts.Tracks; t++ {
		perTrack[t] = g.genTrack(rng, &opts, t)
	}
	// Round-robin interleave tracks so the merger sees all tracks
	// concurrently, then append the truncated tail.
	var stream []byte
	maxLen := 0
	for _, items := range perTrack {
		if len(items) > maxLen {
			maxLen = len(items)
		}
	}
	for i := 0; i < maxLen; i++ {
		for t := range perTrack {
			if i >= len(perTrack[t]) {
				continue
			}
			it := perTrack[t][i]
			if it.isSR {
				stream = appendRecord(stream, RecRTCP, uint32(t), MarshalSenderReport(it.sr))
			} else {
				stream = appendRecord(stream, RecRTP, uint32(t), it.pkt)
			}
		}
	}
	for i := 0; i < opts.TruncateTail; i++ {
		stream = append(stream, byte(0xA5-i))
	}
	g.Stream = stream
	return g, nil
}

func (g *Generated) genTrack(rng *rand.Rand, opts *GenOptions, track int) []genItem {
	n := opts.PacketsPerTrack
	seq0 := uint16(rng.Intn(1 << 16))
	if opts.SeqNearWrap {
		seq0 = 65500
	}
	ts0 := rng.Uint32()
	ssrc := uint32(track+1)*1000 + 7
	const baseNTP = int64(1_700_000_000)

	// Tracks record the same wall-clock session: NTP domains are aligned
	// (small per-track start offsets), while each track's RTP timestamp
	// domain starts at a random offset, as real senders do.
	trackStart := int64(track)*3 + int64(rng.Intn(3))
	srFor := func(sentIdx int, jump bool) *SenderReport {
		// NTP derives from the wall clock; the RTP timestamp field is
		// the on-the-wire 32-bit value from the track's own domain.
		totalTicks := uint64(sentIdx) * TicksPerPacket
		ntp := baseNTP + trackStart + int64(totalTicks/ClockRate)
		if jump {
			ntp += opts.ClockJumpSeconds
		}
		return &SenderReport{
			SSRC:         ssrc,
			NTPSec:       uint32(ntp),
			NTPFrac:      uint32(((totalTicks % ClockRate) << 32) / ClockRate),
			RTPTimestamp: ts0 + uint32(sentIdx)*TicksPerPacket,
			PacketCount:  uint32(sentIdx),
			OctetCount:   uint32(sentIdx * opts.PayloadSize),
		}
	}

	// Build the item list: leading SR, packets, periodic SRs, final SR.
	var items []genItem
	jumpFrom := n / 2
	jump := func(idx int) bool {
		return opts.ClockJumpTrack == track && idx >= jumpFrom
	}
	items = append(items, genItem{isSR: true, sr: srFor(0, false), idx: -1})
	for i := 0; i < n; i++ {
		payload := make([]byte, opts.PayloadSize)
		rng.Read(payload)
		pkt := &rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				PayloadType:    96,
				SequenceNumber: seq0 + uint16(i),
				Timestamp:      ts0 + uint32(i)*TicksPerPacket,
				SSRC:           ssrc,
			},
			Payload: payload,
		}
		raw, _ := pkt.Marshal()
		items = append(items, genItem{pkt: raw, idx: i})
		if (i+1)%SRInterval == 0 {
			items = append(items, genItem{isSR: true, sr: srFor(i+1, jump(i+1)), idx: -1})
		}
	}
	items = append(items, genItem{isSR: true, sr: srFor(n, jump(n)), idx: -1})
	g.SentPerTrack[track] = uint32(n)

	// Impairments, RTP items only.
	items = applyLoss(rng, items, opts, n)
	items = applyReorder(rng, items, opts.ReorderWindow)
	items = applyDupAndCorrupt(rng, items, opts, &g.RTPRecords)
	return items
}

func applyLoss(rng *rand.Rand, items []genItem, opts *GenOptions, n int) []genItem {
	tailFrom := n - opts.TailLoss
	if tailFrom < 0 {
		tailFrom = 0
	}
	out := items[:0]
	for _, it := range items {
		if it.isSR {
			out = append(out, it)
			continue
		}
		if it.idx >= tailFrom || rng.Float64() < opts.LossRate {
			continue
		}
		out = append(out, it)
	}
	return out
}

func applyReorder(rng *rand.Rand, items []genItem, window int) []genItem {
	if window <= 1 {
		return items
	}
	for lo := 0; lo < len(items); lo += window {
		hi := lo + window
		if hi > len(items) {
			hi = len(items)
		}
		rng.Shuffle(hi-lo, func(a, b int) {
			items[lo+a], items[lo+b] = items[lo+b], items[lo+a]
		})
	}
	return items
}

func applyDupAndCorrupt(rng *rand.Rand, items []genItem, opts *GenOptions, rtpCount *int) []genItem {
	var out []genItem
	for _, it := range items {
		out = append(out, it)
		if it.isSR {
			continue
		}
		*rtpCount++
		if rng.Float64() < opts.DupRate {
			out = append(out, it)
			*rtpCount++
		}
		if rng.Float64() < opts.CorruptRate {
			bad := make([]byte, len(it.pkt))
			copy(bad, it.pkt)
			bad[0] = 3<<6 | bad[0]&0x3F // invalid RTP version 3
			out = append(out, genItem{pkt: bad, idx: it.idx})
			*rtpCount++
		}
	}
	return out
}

// appendRecord frames one record: [type:1][track:4 BE][length:2 BE][payload].
func appendRecord(buf []byte, typ byte, track uint32, payload []byte) []byte {
	var hdr [HeaderLen]byte
	hdr[0] = typ
	binary.BigEndian.PutUint32(hdr[1:5], track)
	binary.BigEndian.PutUint16(hdr[5:7], uint16(len(payload)))
	buf = append(buf, hdr[:]...)
	return append(buf, payload...)
}
