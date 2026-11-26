package manifest

// PacketsForStream returns the declared packets of one stream in file order.
func (m *Manifest) PacketsForStream(id int) []Packet {
	out := make([]Packet, 0)
	for _, p := range m.Packets {
		if p.StreamID == id {
			out = append(out, p)
		}
	}
	return out
}

// MediaPackets filters out parity packets.
func MediaPackets(pkts []Packet) []Packet {
	out := make([]Packet, 0, len(pkts))
	for _, p := range pkts {
		if p.Kind == KindMedia {
			out = append(out, p)
		}
	}
	return out
}

// GroupPackets indexes one stream's packets by XOR check group id.
func GroupPackets(pkts []Packet) map[int64][]Packet {
	groups := make(map[int64][]Packet)
	for _, p := range pkts {
		groups[p.GroupID] = append(groups[p.GroupID], p)
	}
	return groups
}

// MaxSeq returns the highest media sequence number for a stream, or -1.
func MaxSeq(pkts []Packet) int64 {
	var max int64 = -1
	for _, p := range pkts {
		if p.Kind == KindMedia && p.Seq > max {
			max = p.Seq
		}
	}
	return max
}

// FindPacket locates a packet by stream and sequence.
func FindPacket(pkts []Packet, streamID int, seq int64) (Packet, bool) {
	for _, p := range pkts {
		if p.StreamID == streamID && p.Seq == seq && p.Kind == KindMedia {
			return p, true
		}
	}
	return Packet{}, false
}

// AnchorsForStream returns the sync anchors declared for one stream.
func (m *Manifest) AnchorsForStream(id int) []Anchor {
	out := make([]Anchor, 0)
	for _, a := range m.Anchors {
		if a.StreamID == id {
			out = append(out, a)
		}
	}
	return out
}

// StreamKind reports the kind of a stream id, or "" when unknown.
func (m *Manifest) StreamKind(id int) string {
	if s, ok := m.StreamByID(id); ok {
		return s.Kind
	}
	return ""
}
