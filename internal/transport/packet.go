package transport

import (
	"hash/fnv"

	"mediamix/internal/timeline"
)

// Limits bounds a single track's reorder state. The number of tracks is
// bounded by the timeline graph, which owns graph-wide policy.
type Limits struct {
	MaxBuffered int
}

// DefaultLimits are deliberately small: a proxy stream must not pin unbounded
// memory while a weak uplink is being repaired.
func DefaultLimits() Limits { return Limits{MaxBuffered: 2048} }

// Packet is one depacketized RTP packet on the shared media timeline.
type Packet struct {
	Track      string
	SSRC       uint32
	Seq        uint16
	Ext        uint64
	Timestamp  uint32
	Marker     bool
	Keyframe   bool
	Payload    []byte
	ArrivalUs  timeline.Micros
	Retransmit bool
}

// digest identifies payload identity for duplicate-conflict detection.
func (p Packet) digest() uint64 {
	h := fnv.New64a()
	var hdr [8]byte
	for i := 0; i < 4; i++ {
		hdr[i] = byte(p.Timestamp >> (8 * (3 - i)))
	}
	hdr[4] = byte(p.Seq >> 8)
	hdr[5] = byte(p.Seq)
	if p.Marker {
		hdr[6] = 1
	}
	if p.Keyframe {
		hdr[7] = 1
	}
	_, _ = h.Write(hdr[:])
	_, _ = h.Write(p.Payload)
	return h.Sum64()
}

// Stats accumulates per-track transport counters.
type Stats struct {
	Received     int `json:"received"`
	Delivered    int `json:"delivered"`
	Duplicates   int `json:"duplicates"`
	Conflicts    int `json:"conflicts"`
	Late         int `json:"late"`
	Reordered    int `json:"reordered"`
	Timeouts     int `json:"timeouts"`
	Skipped      int `json:"skipped_packets"`
	PeakBuffered int `json:"peak_buffered"`
}

// Gap describes the single contiguous run of missing extended sequence numbers
// currently blocking delivery.
type Gap struct {
	Track    string          `json:"track"`
	StartExt uint64          `json:"start_ext"`
	EndExt   uint64          `json:"end_ext"` // inclusive
	Count    int             `json:"count"`
	SinceUs  timeline.Micros `json:"since_us"`
}
