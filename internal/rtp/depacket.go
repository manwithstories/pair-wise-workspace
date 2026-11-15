package rtp

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash"

	pionrtp "github.com/pion/rtp"
)

const maxNALUSize = 8 << 20

// DepacketStats is the bounded set of frame metrics retained for a session.
type DepacketStats struct {
	FramesReassembled int      `json:"frames_reassembled"`
	DroppedFrames     int      `json:"dropped_frames"`
	Keyframes         int      `json:"keyframes"`
	Bytes             int      `json:"bytes"`
	FrameHashes       []string `json:"frame_hashes"`
	MalformedPackets  int      `json:"malformed_packets,omitempty"`
}

type accessUnit struct {
	timestamp uint32
	hasher    hash.Hash
	nalus     int
	bytes     int
	keyframe  bool
	damaged   bool
}

// Depacketizer reconstructs H.264 access units from a single RTP stream.
type Depacketizer struct {
	current  *accessUnit
	fuActive bool
	fuNAL    []byte
	stats    DepacketStats
	firstErr error
}

// NewDepacketizer creates a streaming H.264 depacketizer.
func NewDepacketizer() *Depacketizer {
	return &Depacketizer{stats: DepacketStats{FrameHashes: make([]string, 0, 3)}}
}

// PushOrdered consumes a reordered packet and an optional preceding gap.
func (d *Depacketizer) PushOrdered(packet OrderedPacket) error {
	if packet.Packet == nil {
		return nil
	}
	if packet.GapBefore > 0 {
		d.markGap()
	}
	pkt := packet.Packet
	if d.current != nil && d.current.timestamp != pkt.Timestamp {
		d.finalize(false)
	}
	if d.current == nil {
		d.current = &accessUnit{timestamp: pkt.Timestamp, hasher: sha256.New()}
	}
	if err := d.consumePayload(pkt.Payload); err != nil {
		d.current.damaged = true
		d.fuActive = false
		d.setError(err)
	}
	if pkt.Marker {
		if d.fuActive {
			d.current.damaged = true
		}
		d.finalize(true)
	}
	return d.firstErr
}

// Push is shorthand for PushOrdered when gap information is not available.
func (d *Depacketizer) Push(pkt *pionrtp.Packet, gapBefore int) error {
	return d.PushOrdered(OrderedPacket{Packet: pkt, GapBefore: gapBefore})
}

func (d *Depacketizer) markGap() {
	if d.current != nil {
		d.current.damaged = true
	}
	d.fuActive = false
	d.fuNAL = nil
}

func (d *Depacketizer) consumePayload(payload []byte) error {
	if len(payload) == 0 {
		return fmt.Errorf("empty H.264 RTP payload")
	}
	switch typ := payload[0] & 0x1f; typ {
	case 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23:
		if d.fuActive {
			d.current.damaged = true
			d.fuActive = false
		}
		return d.appendNAL(payload)
	case 24:
		if d.fuActive {
			d.current.damaged = true
			d.fuActive = false
		}
		return d.consumeSTAPA(payload[1:])
	case 28:
		return d.consumeFUA(payload)
	default:
		return fmt.Errorf("unsupported H.264 RTP payload type %d", typ)
	}
}

func (d *Depacketizer) consumeSTAPA(body []byte) error {
	if len(body) == 0 {
		return fmt.Errorf("empty STAP-A payload")
	}
	count := 0
	for len(body) > 0 {
		if len(body) < 2 {
			return fmt.Errorf("truncated STAP-A NAL size")
		}
		size := int(binary.BigEndian.Uint16(body[:2]))
		body = body[2:]
		if size == 0 || size > len(body) {
			return fmt.Errorf("invalid STAP-A NAL size %d", size)
		}
		if err := d.appendNAL(body[:size]); err != nil {
			return err
		}
		body = body[size:]
		count++
	}
	if count == 0 {
		return fmt.Errorf("STAP-A contains no NAL units")
	}
	return nil
}

func (d *Depacketizer) consumeFUA(payload []byte) error {
	if len(payload) < 2 {
		return fmt.Errorf("truncated FU-A payload")
	}
	indicator, fuHeader := payload[0], payload[1]
	start, end := fuHeader&0x80 != 0, fuHeader&0x40 != 0
	nalType := fuHeader & 0x1f
	if nalType == 0 || nalType == 24 || nalType == 28 || nalType == 29 {
		return fmt.Errorf("invalid FU-A NAL type %d", nalType)
	}
	if start {
		if d.fuActive {
			d.current.damaged = true
		}
		d.fuNAL = []byte{(indicator & 0xe0) | nalType}
		d.fuActive = true
	} else if !d.fuActive {
		d.current.damaged = true
		return nil
	}
	if len(d.fuNAL)+len(payload)-2 > maxNALUSize {
		return fmt.Errorf("FU-A NAL exceeds size limit")
	}
	d.fuNAL = append(d.fuNAL, payload[2:]...)
	if end {
		nal := d.fuNAL
		d.fuNAL = nil
		d.fuActive = false
		return d.appendNAL(nal)
	}
	return nil
}

func (d *Depacketizer) appendNAL(nal []byte) error {
	if len(nal) == 0 {
		return fmt.Errorf("empty NAL unit")
	}
	d.current.hasher.Write([]byte{0, 0, 0, 1})
	d.current.hasher.Write(nal)
	d.current.nalus++
	d.current.bytes += 4 + len(nal)
	if nal[0]&0x1f == 5 {
		d.current.keyframe = true
	}
	return nil
}

func (d *Depacketizer) finalize(complete bool) {
	au := d.current
	d.current = nil
	d.fuActive = false
	d.fuNAL = nil
	if au == nil {
		return
	}
	if !complete || au.damaged || au.nalus == 0 {
		d.stats.DroppedFrames++
		return
	}
	d.stats.FramesReassembled++
	d.stats.Bytes += au.bytes
	if au.keyframe {
		d.stats.Keyframes++
	}
	if len(d.stats.FrameHashes) < 3 {
		d.stats.FrameHashes = append(d.stats.FrameHashes, hashPrefix(au.hasher.Sum(nil)))
	}
}

func (d *Depacketizer) setError(err error) {
	d.stats.MalformedPackets++
	if d.firstErr == nil {
		d.firstErr = err
	}
}

// Finish drops an unterminated final access unit and releases buffered state.
func (d *Depacketizer) Finish() {
	if d.current != nil {
		d.finalize(false)
	}
}

// Stats returns a copy of the metrics gathered so far.
func (d *Depacketizer) Stats() DepacketStats {
	out := d.stats
	out.FrameHashes = append([]string(nil), d.stats.FrameHashes...)
	return out
}

// Err returns the first malformed-payload error.
func (d *Depacketizer) Err() error { return d.firstErr }

func hashPrefix(sum []byte) string { return hex.EncodeToString(sum)[:16] }
