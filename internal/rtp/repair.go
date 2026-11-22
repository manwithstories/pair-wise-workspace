package rtp

// Result classifies the outcome of repairing one release.
type Result struct {
	Packets       []*Packet   // originals plus recovered, in extended order
	Recovered     int         // packets synthesised from redundancy/neighbours
	Recoverable   int         // lost packets inside the repair window
	Unrecoverable [][2]int64  // extended ranges that could not be repaired
	LostRaw       [][2]uint16 // original 16-bit sequence ranges lost
}

// Repair fills declared-loss gaps in a release. An isolated loss (gap of
// one packet) is classed recoverable and is repaired from the following
// packet's redundancy payload when present, otherwise by interpolating
// between its neighbours. Bursts longer than the redundancy depth are
// reported unrecoverable and must be marked, never silently committed.
func Repair(rel *Release) *Result {
	res := &Result{}
	byExt := make(map[int64]*Packet, len(rel.Packets))
	for _, p := range rel.Packets {
		byExt[p.ExtSeq] = p
	}
	for _, g := range rel.Gaps {
		res.LostRaw = append(res.LostRaw, [2]uint16{uint16(g[0]), uint16(g[1])})
		if g[0] == g[1] {
			res.Recoverable++
			ext := g[0]
			prev, hasPrev := byExt[ext-1]
			next, hasNext := byExt[ext+1]
			switch {
			case hasNext && next.Redundant != nil:
				byExt[ext] = synthesise(next, ext, next.Redundant)
				res.Recovered++
			case hasPrev && hasNext:
				byExt[ext] = synthesise(prev, ext, prev.Payload)
				res.Recovered++
			default:
				res.Unrecoverable = appendRange(res.Unrecoverable, ext)
			}
			continue
		}
		for ext := g[0]; ext <= g[1]; ext++ {
			res.Unrecoverable = appendRange(res.Unrecoverable, ext)
		}
	}
	// Emit in extended order across packets, repairs and gaps.
	var first, last int64
	init := false
	consider := func(ext int64) {
		if !init {
			first, last, init = ext, ext, true
			return
		}
		if ext < first {
			first = ext
		}
		if ext > last {
			last = ext
		}
	}
	for ext := range byExt {
		consider(ext)
	}
	for _, g := range rel.Gaps {
		consider(g[0])
		consider(g[1])
	}
	for ext := first; ext <= last; ext++ {
		if p, ok := byExt[ext]; ok {
			res.Packets = append(res.Packets, p)
		}
	}
	return res
}

func synthesise(neighbour *Packet, ext int64, payload []byte) *Packet {
	return &Packet{
		Track:     neighbour.Track,
		Seq:       uint16(ext),
		ExtSeq:    ext,
		Timestamp: neighbour.Timestamp,
		Payload:   append([]byte(nil), payload...),
		Arrival:   neighbour.Arrival,
		Recovered: true,
	}
}

func appendRange(ranges [][2]int64, ext int64) [][2]int64 {
	if n := len(ranges); n > 0 && ranges[n-1][1] == ext-1 {
		ranges[n-1][1] = ext
		return ranges
	}
	return append(ranges, [2]int64{ext, ext})
}
