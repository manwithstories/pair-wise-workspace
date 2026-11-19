package testkit

import (
	"encoding/binary"
	"os"

	pionrtp "github.com/pion/rtp"

	"archivearchivist/internal/model"
	"archivearchivist/internal/rtp"
)

type captureManifest struct {
	lost      []uint16
	late      []uint16
	dupes     int
	reordered int
	wraps     int
}

// generateCapture emits one RTP stream for track V1. Timestamps begin 3 s
// before the 33-bit boundary so exactly one wrap occurs. The capture order is
// scrambled within a bounded window, contains duplicates, and drops packets
// that never reappear (true loss) plus a packet that reappears too late.
func generateCapture(cfg Config, rnd *rand) ([]captureRecord, captureManifest, error) {
	var m captureManifest
	const mod = model.Modulus33
	const rate = 90000
	frames := cfg.VideoFrames
	start := mod - int64(rate)
	type wire struct {
		seq   uint16
		ts    int64 // 33-bit transport clock, wrapped
		extTS int64 // extended (pre-wrap) clock, for the capture record
		size  int
	}
	wires := make([]wire, frames)
	for i := 0; i < frames; i++ {
		ext := start + int64(i)*3000
		wires[i] = wire{
			seq:   uint16(1000 + i),
			ts:    ext % mod,
			extTS: ext,
			size:  480,
		}
	}
	// Wrap count: timestamps crossing the modulus.
	for i := 1; i < frames; i++ {
		if wires[i].ts < wires[i-1].ts {
			m.wraps++
		}
	}

	lost := map[int]bool{}
	for i := 3; i < frames-4; i += cfg.LossEvery {
		lost[i] = true
		m.lost = append(m.lost, wires[i].seq)
	}
	// A late packet: frame 1 is delayed until after the window has passed.
	const lateIdx = 1
	m.late = append(m.late, wires[lateIdx].seq)

	stream := make([]int, 0, frames)
	for i := range wires {
		if lost[i] {
			continue
		}
		stream = append(stream, i)
	}

	// Deliver in capture order: local shuffles of adjacent frames emulate
	// jitter without violating the receive window.
	order := make([]int, len(stream))
	copy(order, stream)
	for i := 0; i+2 < len(order); i += 3 {
		if rnd.intn(100) < 55 {
			order[i], order[i+1] = order[i+1], order[i]
			m.reordered++
		}
	}
	// Pull the late frame to the very end.
	for i, idx := range order {
		if idx == lateIdx {
			order = append(append(order[:i:i], order[i+1:]...), lateIdx)
			break
		}
	}

	recs := make([]captureRecord, 0, len(order)+4)
	// Arrival times follow the media clock with sub-millisecond jitter and a
	// sustained 120 ppm drift. That exceeds the 80 ppm review threshold, so
	// every run exercises the review flag while drift correction keeps the
	// residual sync error in the sub-millisecond range.
	arrival := int64(0)
	for _, idx := range order {
		w := wires[idx]
		mediaUS := (w.extTS - wires[0].extTS) * 1_000_000 / rate
		jitter := int64(rnd.intn(300) - 150)
		arr := mediaUS + mediaUS*120/1_000_000 + jitter
		if arr < arrival {
			arr = arrival
		}
		arrival = arr + 1
		recs = append(recs, captureRecord{seq: w.seq, ts: w.ts, size: w.size, arrival: arr})
		if rnd.intn(100) < 20 && idx != lateIdx {
			// Duplicate of an earlier packet arrives right behind the original.
			recs = append(recs, captureRecord{seq: w.seq, ts: w.ts, size: w.size, arrival: arr + 900})
			m.dupes++
		}
	}
	// Shift arrivals into a realistic epoch so "zero arrival" is never valid.
	for i := range recs {
		recs[i].arrival += 1_700_000_000_000_000
	}
	return recs, m, nil
}

type captureRecord struct {
	seq     uint16
	ts      int64
	size    int
	arrival int64
}

// writeCapture encodes records in the archivist capture format: magic, count,
// then fixed header plus the full RTP packet per record.
func writeCapture(path string, recs []captureRecord) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.WriteString("ARCRTP01"); err != nil {
		return err
	}
	if err := binary.Write(f, binary.BigEndian, uint32(len(recs))); err != nil {
		return err
	}
	for i, r := range recs {
		pkt := &pionrtp.Packet{
			Header: pionrtp.Header{
				Version:        2,
				PayloadType:    96,
				SequenceNumber: r.seq,
				Timestamp:      uint32(r.ts & 0xffffffff),
				SSRC:           0x12345678,
				Marker:         i == len(recs)-1,
			},
			Payload: make([]byte, r.size),
		}
		ext := make([]byte, 8)
		binary.BigEndian.PutUint64(ext, uint64(r.ts))
		if err := pkt.SetExtension(rtp.ExtIDTimestamp, ext); err != nil {
			return err
		}
		for j := range pkt.Payload {
			pkt.Payload[j] = byte(r.ts + int64(j))
		}
		raw, err := pkt.Marshal()
		if err != nil {
			return err
		}
		if err := binary.Write(f, binary.BigEndian, r.arrival); err != nil {
			return err
		}
		if err := binary.Write(f, binary.BigEndian, r.seq); err != nil {
			return err
		}
		if err := binary.Write(f, binary.BigEndian, uint32(len(raw))); err != nil {
			return err
		}
		if _, err := f.Write(raw); err != nil {
			return err
		}
	}
	return f.Sync()
}
