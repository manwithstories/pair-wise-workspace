package rtp

func (r *Receiver) drain(force bool) []Packet {
	var out []Packet
	for {
		if p, ok := r.buffer[r.next]; ok {
			delete(r.buffer, r.next)
			r.delivered[p.Seq] = true
			if p.Reordered {
				r.stats.Reordered++
			}
			r.stats.Delivered++
			out = append(out, *p)
			r.next++
			continue
		}
		if len(r.buffer) == 0 {
			return out
		}
		limit := r.maxSeen
		if !force {
			w := uint64(r.window)
			if r.maxSeen < r.next+w {
				return out
			}
			limit = r.maxSeen - w
		}
		minSeq := r.lowestBuffered()
		skipTo := minSeq
		if !force && skipTo > limit+1 {
			skipTo = limit + 1
		}
		if skipTo <= r.next {
			// Cannot make progress without exceeding the window.
			return out
		}
		r.markLost(r.next, skipTo)
		r.next = skipTo
	}
}

func (r *Receiver) lowestBuffered() uint64 {
	min := ^uint64(0)
	for s := range r.buffer {
		if s < min {
			min = s
		}
	}
	return min
}

func (r *Receiver) markLost(from, to uint64) {
	for s := from; s < to; s++ {
		if r.delivered[s] {
			continue
		}
		r.lostSet[s] = true
		r.stats.Lost++
		r.stats.MissingSeqs = append(r.stats.MissingSeqs, s)
	}
}
