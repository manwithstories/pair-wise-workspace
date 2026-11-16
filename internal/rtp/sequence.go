package rtpx

import "time"

func nowMonotonic() time.Time { return time.Now() }

// extend turns a 16-bit sequence number into a monotonic counter by choosing
// the epoch nearest the current high-water mark. It reports whether the
// sequence crossed the 16-bit boundary, which is how wraparound is counted.
func (st *streamState) extend(seq uint16) (uint64, bool) {
	base := st.highest &^ (seqModulus - 1)
	ext := base | uint64(seq)
	diff := int64(ext) - int64(st.highest)
	switch {
	case diff < -seqHalf:
		ext += seqModulus
		return ext, ext>>16 != st.highest>>16
	case diff > seqHalf && ext >= seqModulus:
		ext -= seqModulus
	}
	return ext, false
}

func (st *streamState) seenHas(ext uint64) bool {
	_, ok := st.seen[ext]
	return ok
}

// prune bounds the seen-set so its size tracks the reorder window instead of
// the length of the stream.
func (st *streamState) prune() {
	limit := uint64(st.cfg.ReorderWindow + 1)
	if uint64(len(st.seen)) <= limit*2 {
		return
	}
	cutoff := st.highest - limit
	for k := range st.seen {
		if k < cutoff {
			delete(st.seen, k)
		}
	}
}

// recordJitter applies the RFC 3550 section 6.4.1 estimator:
//
//	D = (arrival_i - arrival_{i-1}) - (ts_i - ts_{i-1})
//	J = J + (|D| - J) / 16
//
// D is expressed in RTP clock ticks so the stored J can be converted to
// milliseconds by dividing by the clock rate. Only a packet at the head of the
// stream advances the transit reference; a reordered packet still contributes
// its deviation but must not pull the reference backwards.
func (st *streamState) recordJitter(p Packet, atHead bool) {
	if !st.haveTrans {
		st.lastArr, st.lastTS, st.haveTrans = p.Arrival, p.Timestamp, true
		return
	}
	arrivalDelta := p.Arrival.Sub(st.lastArr).Seconds() * float64(st.cfg.ClockRate)
	rtpDelta := float64(int64(p.Timestamp) - int64(st.lastTS))
	d := arrivalDelta - rtpDelta
	if d < 0 {
		d = -d
	}
	st.jitter += (d - st.jitter) / jitterGainDivisor
	if st.jitter > st.jitterMax {
		st.jitterMax = st.jitter
	}
	if atHead {
		st.lastArr, st.lastTS = p.Arrival, p.Timestamp
	}
}
