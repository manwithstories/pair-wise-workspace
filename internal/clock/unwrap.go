package clock

import (
	"errors"
	"sort"
)

// WrapPeriod is the modulus of a 32-bit RTP timestamp (RFC 3550 section 5).
const WrapPeriod uint64 = 1 << 32

// halfPeriod is the largest uniquely-decidable jump magnitude. A delta whose
// absolute value reaches half the period could equally be a forward or a
// backward move, so it must be flagged as ambiguous.
const halfPeriod int64 = 1 << 31

var (
	// ErrEmptyStream is returned when unwrapping a stream with no packets.
	ErrEmptyStream = errors.New("clock: empty RTP packet stream")
	// ErrTickOverflow is returned when an unwrapped timestamp cannot be
	// represented on the unified tick grid.
	ErrTickOverflow = errors.New("clock: unwrapped timestamp overflows tick grid")
)

// Packet is one observed RTP packet in arrival order.
type Packet struct {
	Seq uint16 // RTP sequence number (16-bit, wraps)
	TS  uint32 // RTP timestamp (32-bit, wraps)
}

// PacketTime is the fully resolved timing for one packet.
type PacketTime struct {
	ArrivalIdx   int    // position in the input arrival-order slice
	Seq          uint16 // original sequence number
	SeqWrapped   int64  // sequence number with 16-bit wrap removed
	TS           uint32 // original RTP timestamp
	TSUnwrapped  uint64 // timestamp with 32-bit wrap removed
	Ticks        int64  // TSUnwrapped mapped onto the unified microsecond grid
	Ambiguous    bool   // true when the wrap direction could not be decided
}

// AmbiguitySpan is a contiguous run of packets whose timestamp jumps could
// not be uniquely resolved. Positions reference the arrival-order slice.
type AmbiguitySpan struct {
	ArrivalStart int
	ArrivalEnd   int
	SeqStart     uint16
	SeqEnd       uint16
}

// UnwrapResult is the output of timestamp unwrapping.
type UnwrapResult struct {
	Packets     []PacketTime    // kept in arrival order
	Ambiguities []AmbiguitySpan // grouped ambiguous runs
}

// UnwrapConfig parameterizes unwrapping.
type UnwrapConfig struct {
	// ClockRate is the RTP media clock rate (e.g. 90000 for video).
	ClockRate int64
	// AmbiguityThreshold: jumps with |delta| >= this are marked ambiguous.
	// Zero defaults to halfPeriod (2^31), the mathematically correct value.
	AmbiguityThreshold int64
}

// ext is a packet with its 16-bit sequence number extended to 64 bits.
type ext struct {
	arrival int
	seqU    int64
	seq     uint16
	ts      uint32
}

// UnwrapTimestamps removes 16-bit sequence wrap and 32-bit timestamp wrap.
//
// It is a two-pass algorithm: pass one walks packets in arrival order to
// extend sequence numbers, so out-of-order delivery is captured correctly;
// pass two processes timestamps in *send* (sequence) order, which means a
// reordered packet can never be mistaken for a wrap. A jump whose signed
// delta reaches half the 32-bit period is not uniquely decidable and is
// reported as an ambiguity span rather than guessed.
func UnwrapTimestamps(pkts []Packet, cfg UnwrapConfig) (*UnwrapResult, error) {
	if len(pkts) == 0 {
		return nil, ErrEmptyStream
	}
	rate, err := Rate(cfg.ClockRate)
	if err != nil {
		return nil, err
	}
	threshold := cfg.AmbiguityThreshold
	if threshold <= 0 {
		threshold = halfPeriod
	}

	// Pass 1: extend 16-bit sequence numbers in arrival order.
	rows := make([]ext, len(pkts))
	var prevSeq int64
	for i, p := range pkts {
		if i > 0 {
			delta := int16(p.Seq - pkts[i-1].Seq) // signed: handles 16-bit wrap
			prevSeq += int64(delta)
		} else {
			prevSeq = int64(p.Seq)
		}
		rows[i] = ext{arrival: i, seqU: prevSeq, seq: p.Seq, ts: p.TS}
	}

	// Pass 2: send order for timestamp resolution.
	sendOrder := make([]int, len(rows))
	for i := range sendOrder {
		sendOrder[i] = i
	}
	sort.SliceStable(sendOrder, func(a, b int) bool {
		return rows[sendOrder[a]].seqU < rows[sendOrder[b]].seqU
	})

	res := &UnwrapResult{Packets: make([]PacketTime, len(pkts))}
	var prevTSU uint64
	inAmbiguity := false
	ambiguousArrivals := make([]int, 0)
	for ord, idx := range sendOrder {
		row := rows[idx]
		var tsu uint64
		var ambiguous bool
		var delta int64
		if ord == 0 {
			tsu = uint64(row.ts)
		} else {
			prevRow := rows[sendOrder[ord-1]]
			delta = int64(int32(row.ts - prevRow.ts)) // signed 32-bit delta
			// Signed extension: subtract on backward deltas so that, in
			// uint64 space, a reordered/retried packet lands below the
			// previous timestamp instead of wrapping to a huge value.
			if delta >= 0 {
				tsu = prevTSU + uint64(delta)
			} else {
				tsu = prevTSU - uint64(-delta)
			}
			if abs64(delta) >= threshold {
				inAmbiguity = true
			}
			// Once an undecidable jump occurs, absolute positioning can no
			// longer be trusted; every later packet stays in the span.
			ambiguous = inAmbiguity
		}
		var ticks int64
		if !ambiguous {
			v64, err := int64Check(tsu)
			if err != nil {
				return nil, err
			}
			ticks, err = rate.Scale(v64)
			if err != nil {
				return nil, err
			}
		}
		res.Packets[idx] = PacketTime{
			ArrivalIdx:  idx,
			Seq:         row.seq,
			SeqWrapped:  row.seqU,
			TS:          row.ts,
			TSUnwrapped: tsu,
			Ticks:       ticks,
			Ambiguous:   ambiguous,
		}
		prevTSU = tsu
		if ambiguous {
			ambiguousArrivals = append(ambiguousArrivals, idx)
		}
	}
	res.Ambiguities = buildSpans(rows, ambiguousArrivals)
	return res, nil
}

// buildSpans groups ambiguous packet arrival indices into contiguous runs in
// send order and reports each run's boundaries keyed to arrival positions.
func buildSpans(rows []ext, arrivals []int) []AmbiguitySpan {
	if len(arrivals) == 0 {
		return nil
	}
	sort.Ints(arrivals)
	var spans []AmbiguitySpan
	start := arrivals[0]
	prev := arrivals[0]
	for i := 1; i < len(arrivals); i++ {
		cur := arrivals[i]
		// Contiguity is defined in send (unwrapped seq) order.
		if rows[cur].seqU != rows[prev].seqU+1 {
			spans = append(spans, AmbiguitySpan{
				ArrivalStart: start, ArrivalEnd: prev,
				SeqStart: rows[start].seq, SeqEnd: rows[prev].seq,
			})
			start = cur
		}
		prev = cur
	}
	spans = append(spans, AmbiguitySpan{
		ArrivalStart: start, ArrivalEnd: prev,
		SeqStart: rows[start].seq, SeqEnd: rows[prev].seq,
	})
	return spans
}

// int64Check converts an unwrapped uint64 timestamp to int64 for tick
// conversion, refusing values that would overflow the signed grid.
func int64Check(v uint64) (int64, error) {
	const maxInt64 = uint64(1<<63 - 1)
	if v > maxInt64 {
		return 0, ErrTickOverflow
	}
	return int64(v), nil
}
