package rtprepair

import "fmt"

// Stats summarises what the repairer observed.
type Stats struct {
	Received         int   `json:"received"`
	Emitted          int   `json:"emitted"`
	DuplicatePackets int   `json:"duplicate_packets"`
	ReorderedPackets int   `json:"reordered_packets"`
	LostPackets      int   `json:"lost_packets"`
	BurstLostPackets int   `json:"burst_lost_packets"`
	LatePackets      int   `json:"late_packets"`
	MalformedPackets int   `json:"malformed_packets"`
	MaxBufferDepth   int   `json:"max_buffer_depth"`
	RoundtripOK      int   `json:"roundtrip_ok"`
	RoundtripTotal   int   `json:"roundtrip_total"`
	ForcedFlushes    int   `json:"forced_flushes"`
	FirstSeq         int64 `json:"first_seq"`
	LastSeq          int64 `json:"last_seq"`
}

// Consistency returns the byte-level roundtrip ratio in [0,1]; 1 when nothing
// was parsed.
func (s Stats) Consistency() float64 {
	if s.RoundtripTotal == 0 {
		return 1
	}
	return float64(s.RoundtripOK) / float64(s.RoundtripTotal)
}

// Summary formats the aggregate counters for terminal output.
func (s Stats) Summary() string {
	return fmt.Sprintf("received=%d emitted=%d dup=%d reordered=%d lost=%d burst_lost=%d",
		s.Received, s.Emitted, s.DuplicatePackets, s.ReorderedPackets, s.LostPackets, s.BurstLostPackets)
}
