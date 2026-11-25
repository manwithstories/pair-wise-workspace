// Package transport removes RTP framing, duplicates and reordering before TS
// ingestion. The recovered byte stream is passed to the same ingest path used
// for local .ts files.
package transport

import (
	"github.com/pion/rtp"

	"mediad/internal/mediaerr"
	"mediad/internal/timeline"
)

// DefaultReorderWindow bounds memory and latency for packet reordering.
const DefaultReorderWindow = 32

// Stats reports transport recovery actions.
type Stats struct {
	Received   int
	Duplicates int
	Reordered  int
	Late       int
	Released   int
}

// Depacketizer deduplicates and orders RTP packets carrying MPEG-TS payloads.
type Depacketizer struct {
	ssrc     uint32
	haveSSRC bool
	nextSeq  uint16
	haveNext bool
	window   int
	queued   map[uint16]*rtp.Packet
	seen     map[uint16]struct{}
	clock    *timeline.Clock
	Stats    Stats
}

// NewDepacketizer creates a bounded RTP reorder buffer.
func NewDepacketizer(clockRate uint32, window int) *Depacketizer {
	if window <= 0 || window > DefaultReorderWindow {
		window = DefaultReorderWindow
	}
	return &Depacketizer{
		window: window,
		queued: make(map[uint16]*rtp.Packet),
		seen:   make(map[uint16]struct{}),
		clock:  timeline.NewClock(clockRate),
	}
}

// Drift returns the arrival-derived clock estimate.
func (d *Depacketizer) Drift() timeline.Drift { return d.clock.Estimate() }

// PushBytes parses a raw RTP packet then calls Push.
func (d *Depacketizer) PushBytes(raw []byte, arrivalNS int64) ([][]byte, timeline.Drift, error) {
	p := &rtp.Packet{}
	if err := p.Unmarshal(raw); err != nil {
		return nil, d.clock.Estimate(), mediaerr.New(mediaerr.KindInvalidPacket, -1, 0, true, "RTP unmarshal: %v", err)
	}
	out, drift, err := d.Push(p, arrivalNS)
	return out, drift, err
}

// Push accepts one parsed RTP packet and returns any payloads now contiguous.
func (d *Depacketizer) Push(p *rtp.Packet, arrivalNS int64) ([][]byte, timeline.Drift, error) {
	d.Stats.Received++
	drift := d.clock.Observe(p.Timestamp, arrivalNS)

	if d.haveSSRC {
		if p.Header.SSRC != d.ssrc {
			return nil, drift, mediaerr.New(mediaerr.KindInvalidPacket, -1, 0, true,
				"RTP SSRC changed from %08x to %08x", d.ssrc, p.Header.SSRC)
		}
	} else {
		d.ssrc = p.Header.SSRC
		d.haveSSRC = true
	}

	seq := p.Header.SequenceNumber
	if _, ok := d.seen[seq]; ok {
		d.Stats.Duplicates++
		return nil, drift, nil
	}
	if d.haveNext && seqBefore(seq, d.nextSeq) {
		d.Stats.Late++
		d.seen[seq] = struct{}{}
		return nil, drift, nil
	}

	if !d.haveNext {
		d.haveNext = true
		d.nextSeq = seq
	}

	if seq == d.nextSeq {
		d.seen[seq] = struct{}{}
		var out [][]byte
		out = append(out, p.Payload)
		d.Stats.Released++
		d.nextSeq = seq + 1
		d.releaseQueued(&out)
		return out, drift, nil
	}

	// A missing predecessor: queue within the bounded window.
	distance := int(seq - d.nextSeq) // wrapping subtraction is intentional
	if distance <= 0 || distance > d.window {
		return nil, drift, mediaerr.New(mediaerr.KindBufferLimit, -1, 0, true,
			"RTP sequence gap %d exceeds bounded reorder window %d", distance, d.window)
	}
	if len(d.queued) >= d.window {
		return nil, drift, mediaerr.New(mediaerr.KindBufferLimit, -1, 0, true,
			"RTP reorder window full at %d packets", d.window)
	}
	if _, exists := d.queued[seq]; exists {
		d.Stats.Duplicates++
		return nil, drift, nil
	}
	d.queued[seq] = p
	d.seen[seq] = struct{}{}
	d.Stats.Reordered++
	return nil, drift, nil
}

func (d *Depacketizer) releaseQueued(out *[][]byte) {
	for {
		p, ok := d.queued[d.nextSeq]
		if !ok {
			break
		}
		delete(d.queued, d.nextSeq)
		*out = append(*out, p.Payload)
		d.Stats.Released++
		d.nextSeq++
	}
}

// Flush releases queued payloads in sequence order at end of stream. Missing
// packets remain missing; callers feed returned bytes through TS resync.
func (d *Depacketizer) Flush() [][]byte {
	var out [][]byte
	for len(d.queued) > 0 {
		p, ok := d.queued[d.nextSeq]
		if !ok {
			// Skip one missing sequence and try the next within the window.
			d.nextSeq++
			continue
		}
		delete(d.queued, d.nextSeq)
		out = append(out, p.Payload)
		d.nextSeq++
	}
	return out
}

func seqBefore(a, b uint16) bool {
	const half = uint16(1) << 15
	return a != b && b-a < half
}
