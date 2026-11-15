package es

import (
	"fmt"
	"sort"

	"github.com/asticode/go-astits"

	"tsprobe/internal/psi"
	"tsprobe/internal/tspkt"
)

type Bucket struct {
	ProgramNumber   uint16
	PMTPID          uint16
	PMTVersion      uint8
	PMTRevision     int
	Effective       int64
	PID             uint16
	StreamType      uint8
	Packets         int64
	PayloadBytes    int64
	PESStarts       int64
	PCRPackets      int64
	CCPackets       int64
	CCAhead         int64
	CCBackwards     int64
	Duplicates      int64
	Discontinuities int64
	PESTruncated    int64
	LastCC          int
	HasPTS          bool
	FirstPTS        int64
	LastPTS         int64
	HasDTS          bool
	FirstDTS        int64
	LastDTS         int64
}

type PIDStats struct {
	PID             uint16
	TotalPackets    int64
	PayloadPackets  int64
	PayloadBytes    int64
	PESStarts       int64
	PCRPackets      int64
	CCAhead         int64
	CCBackwards     int64
	Duplicates      int64
	Discontinuities int64
	LastCC          int
}

type Totals struct {
	Packets         int64
	PayloadBytes    int64
	PSIPackets      int64
	NullPackets     int64
	OtherPackets    int64
	ESPackets       int64
	CCAhead         int64
	CCBackwards     int64
	Duplicates      int64
	Discontinuities int64
	PESTruncated    int64
}

type bucketKey struct {
	program    uint16
	pmtPID     uint16
	version    uint8
	revision   int
	effective  int64
	pid        uint16
	streamType uint8
}

type pidState struct {
	stats         PIDStats
	hasPayloadCC  bool
	lastPayloadCC uint8
}

type bucketState struct {
	key    bucketKey
	bucket Bucket
}

type pesState struct {
	key      bucketKey
	total    int64
	consumed int64
	bounded  bool
}

type Counter struct {
	pids      map[uint16]*pidState
	buckets   map[bucketKey]*bucketState
	activePES map[uint16]*pesState
	totals    Totals
}

func NewCounter() *Counter {
	return &Counter{
		pids:      make(map[uint16]*pidState),
		buckets:   make(map[bucketKey]*bucketState),
		activePES: make(map[uint16]*pesState),
	}
}

func (c *Counter) Observe(pkt tspkt.Packet, assignment psi.Assignment, assigned, isPSI bool) error {
	c.totals.Packets++
	pid := c.pid(pkt.PID)
	pid.stats.TotalPackets++
	pid.stats.LastCC = int(pkt.CC)
	payloadBytes := len(pkt.Payload)
	if payloadBytes > 0 {
		pid.stats.PayloadBytes += int64(payloadBytes)
		c.totals.PayloadBytes += int64(payloadBytes)
	}
	if pkt.HasPCR {
		pid.stats.PCRPackets++
	}

	var bucket *bucketState
	switch {
	case pkt.PID == astits.PIDNull:
		c.totals.NullPackets++
	case isPSI:
		c.totals.PSIPackets++
	case assigned:
		c.totals.ESPackets++
		bucket = c.bucketFor(assignment)
	default:
		c.totals.OtherPackets++
	}
	if bucket != nil {
		bucket.bucket.Packets++
		bucket.bucket.PayloadBytes += int64(payloadBytes)
		bucket.bucket.LastCC = int(pkt.CC)
		if pkt.HasPCR {
			bucket.bucket.PCRPackets++
		}
	}

	if pkt.Discontinuity {
		pid.stats.Discontinuities++
		c.totals.Discontinuities++
		if bucket != nil {
			bucket.bucket.Discontinuities++
		}
		pid.hasPayloadCC = false
	}

	hasPayload := pkt.HasPayload && payloadBytes > 0
	if hasPayload {
		pid.stats.PayloadPackets++
		if bucket != nil {
			bucket.bucket.CCPackets++
		}
		c.observeContinuity(pid, bucket, pkt.CC)
	}
	if hasPayload && assigned && bucket != nil {
		if pkt.PUSI {
			if err := c.startPES(pkt.PID, bucket, pkt.Payload); err != nil {
				return err
			}
		} else {
			c.continuePES(pkt.PID, int64(payloadBytes))
		}
	}
	return nil
}

func (c *Counter) observeContinuity(pid *pidState, bucket *bucketState, cc uint8) {
	if !pid.hasPayloadCC {
		pid.hasPayloadCC = true
		pid.lastPayloadCC = cc
		return
	}
	last := pid.lastPayloadCC
	if cc == last {
		pid.stats.Duplicates++
		c.totals.Duplicates++
		if bucket != nil {
			bucket.bucket.Duplicates++
		}
		return
	}
	diff := (cc - last) & 0x0f
	if diff > 1 && diff <= 8 {
		ahead := int64(diff - 1)
		pid.stats.CCAhead += ahead
		c.totals.CCAhead += ahead
		if bucket != nil {
			bucket.bucket.CCAhead += ahead
		}
	} else if diff > 8 {
		pid.stats.CCBackwards++
		c.totals.CCBackwards++
		if bucket != nil {
			bucket.bucket.CCBackwards++
		}
	}
	pid.lastPayloadCC = cc
}

func (c *Counter) pid(pid uint16) *pidState {
	state := c.pids[pid]
	if state == nil {
		state = &pidState{stats: PIDStats{PID: pid}}
		c.pids[pid] = state
	}
	return state
}

func (c *Counter) bucketFor(a psi.Assignment) *bucketState {
	key := bucketKey{
		program: a.ProgramNumber, pmtPID: a.PMTPID, version: a.PMTVersion,
		revision: a.PMTRevision, effective: a.Effective, pid: a.PID, streamType: a.StreamType,
	}
	bucket := c.buckets[key]
	if bucket == nil {
		bucket = &bucketState{key: key, bucket: Bucket{
			ProgramNumber: a.ProgramNumber, PMTPID: a.PMTPID, PMTVersion: a.PMTVersion,
			PMTRevision: a.PMTRevision, Effective: a.Effective, PID: a.PID, StreamType: a.StreamType,
		}}
		c.buckets[key] = bucket
	}
	return bucket
}

func (c *Counter) Finish() {
	c.finishPES()
}

func (c *Counter) Buckets() []Bucket {
	out := make([]Bucket, 0, len(c.buckets))
	for _, state := range c.buckets {
		out = append(out, state.bucket)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ProgramNumber != out[j].ProgramNumber {
			return out[i].ProgramNumber < out[j].ProgramNumber
		}
		if out[i].PID != out[j].PID {
			return out[i].PID < out[j].PID
		}
		return out[i].PMTRevision < out[j].PMTRevision
	})
	return out
}

func (c *Counter) PIDs() []PIDStats {
	out := make([]PIDStats, 0, len(c.pids))
	for _, state := range c.pids {
		out = append(out, state.stats)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PID < out[j].PID })
	return out
}

func (c *Counter) Totals() Totals { return c.totals }

func (c *Counter) startPES(pid uint16, bucket *bucketState, payload []byte) error {
	if active := c.activePES[pid]; active != nil {
		if active.bounded && active.consumed < active.total {
			c.markPESTruncated(active.key)
		}
		delete(c.activePES, pid)
	}
	total, bounded, pts, dts, hasPTS, hasDTS, err := parsePESHeader(payload)
	if err != nil {
		return fmt.Errorf("es: pid %#x: %w", pid, err)
	}
	bucket.bucket.PESStarts++
	c.pid(pid).stats.PESStarts++
	if hasPTS {
		if !bucket.bucket.HasPTS {
			bucket.bucket.HasPTS = true
			bucket.bucket.FirstPTS = pts
		}
		bucket.bucket.LastPTS = pts
	}
	if hasDTS {
		if !bucket.bucket.HasDTS {
			bucket.bucket.HasDTS = true
			bucket.bucket.FirstDTS = dts
		}
		bucket.bucket.LastDTS = dts
	}
	consumed := int64(len(payload))
	if bounded && consumed >= total {
		return nil
	}
	c.activePES[pid] = &pesState{key: bucket.key, total: total, consumed: consumed, bounded: bounded}
	return nil
}

func (c *Counter) continuePES(pid uint16, n int64) {
	active := c.activePES[pid]
	if active == nil {
		return
	}
	active.consumed += n
	if active.bounded && active.consumed >= active.total {
		delete(c.activePES, pid)
	}
}

func (c *Counter) finishPES() {
	for pid, active := range c.activePES {
		if active.bounded && active.consumed < active.total {
			c.markPESTruncated(active.key)
		}
		delete(c.activePES, pid)
	}
}

func (c *Counter) markPESTruncated(key bucketKey) {
	bucket := c.buckets[key]
	if bucket == nil {
		return
	}
	bucket.bucket.PESTruncated++
	c.totals.PESTruncated++
}

func parsePESHeader(payload []byte) (total int64, bounded bool, pts, dts int64, hasPTS, hasDTS bool, err error) {
	if len(payload) < 9 {
		return 0, false, 0, 0, false, false, fmt.Errorf("PES header is truncated")
	}
	if payload[0] != 0x00 || payload[1] != 0x00 || payload[2] != 0x01 {
		return 0, false, 0, 0, false, false, fmt.Errorf("PES start code is invalid")
	}
	streamID := payload[3]
	if streamID < 0xbc {
		return 0, false, 0, 0, false, false, fmt.Errorf("PES stream_id %#x is invalid", streamID)
	}
	packetLength := int64(payload[4])<<8 | int64(payload[5])
	if payload[6]&0xc0 != 0x80 {
		return 0, false, 0, 0, false, false, fmt.Errorf("PES optional header marker is invalid")
	}
	headerLength := int(payload[8])
	headerEnd := 9 + headerLength
	if headerEnd > len(payload) {
		return 0, false, 0, 0, false, false, fmt.Errorf("PES optional header exceeds TS payload")
	}
	indicator := (payload[7] >> 6) & 0x03
	if indicator == astits.PTSDTSIndicatorIsForbidden {
		return 0, false, 0, 0, false, false, fmt.Errorf("PES PTS/DTS indicator is forbidden")
	}
	if indicator == astits.PTSDTSIndicatorOnlyPTS || indicator == astits.PTSDTSIndicatorBothPresent {
		if headerLength < 5 || len(payload) < 14 {
			return 0, false, 0, 0, false, false, fmt.Errorf("PES PTS is truncated")
		}
		pts, err = decodeTimestamp(payload[9:14])
		if err != nil {
			return 0, false, 0, 0, false, false, err
		}
		hasPTS = true
	}
	if indicator == astits.PTSDTSIndicatorBothPresent {
		if headerLength < 10 || len(payload) < 19 {
			return 0, false, 0, 0, false, false, fmt.Errorf("PES DTS is truncated")
		}
		dts, err = decodeTimestamp(payload[14:19])
		if err != nil {
			return 0, false, 0, 0, false, false, err
		}
		hasDTS = true
	}
	if packetLength == 0 {
		return 0, false, pts, dts, hasPTS, hasDTS, nil
	}
	total = 6 + packetLength
	if total < int64(headerEnd) {
		return 0, false, 0, 0, false, false, fmt.Errorf("PES packet_length %d is shorter than its header", packetLength)
	}
	return total, true, pts, dts, hasPTS, hasDTS, nil
}

func decodeTimestamp(data []byte) (int64, error) {
	if len(data) != 5 {
		return 0, fmt.Errorf("PES timestamp is truncated")
	}
	if data[0]&1 == 0 || data[2]&1 == 0 || data[4]&1 == 0 {
		return 0, fmt.Errorf("PES timestamp marker bit is invalid")
	}
	value := int64(data[0]>>1&0x07)<<30 |
		int64(data[1])<<22 |
		int64(data[2]>>1)<<15 |
		int64(data[3])<<7 |
		int64(data[4]>>1)
	return value, nil
}
