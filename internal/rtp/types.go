// Package rtp receives a packetised media stream, removes duplicates,
// restores order and reports unrecoverable loss. It never invents data: a
// frame that did not arrive is reported as a gap, never synthesised.
package rtp

// Config bounds the reorder buffer.
type Config struct {
	// InitialWindow is the number of sequence numbers the receiver may wait
	// behind the highest sequence seen before declaring a gap lost.
	InitialWindow int
	// MinWindow and MaxWindow clamp adaptive changes.
	MinWindow int
	MaxWindow int
	// MaxBuffered caps buffered packets. Exceeding it is an error, never a
	// silent drop.
	MaxBuffered int
	// ReferenceBandwidthMbps is the bandwidth at which InitialWindow applies.
	ReferenceBandwidthMbps float64
	// ClockRate is the RTP media clock in ticks per second.
	ClockRate uint32
}

// DefaultConfig returns safe defaults for a 90kHz video stream.
func DefaultConfig() Config {
	return Config{
		InitialWindow:          64,
		MinWindow:              8,
		MaxWindow:              2048,
		MaxBuffered:            8192,
		ReferenceBandwidthMbps: 8,
		ClockRate:              90000,
	}
}

// Packet is one de-duplicated packet in stream order.
type Packet struct {
	Seq          uint64
	RTPSeq       uint16
	Timestamp    uint32
	Marker       bool
	PayloadBytes int
	// Arrival is the 0-based index at which the packet was handed to Push.
	Arrival int
	// Reordered is true when the packet arrived after a later sequence
	// number but was still delivered in time.
	Reordered bool
}

// Stats quantifies what the receiver observed.
type Stats struct {
	Received     int           `json:"received"`
	Delivered    int           `json:"delivered"`
	Duplicates   int           `json:"duplicates"`
	Reordered    int           `json:"reordered"`
	Lost         int           `json:"lost"`
	TooLate      int           `json:"too_late"`
	MissingSeqs  []uint64      `json:"missing_sequences"`
	WindowStart  int           `json:"window_start"`
	WindowEnd    int           `json:"window_end"`
	WindowShifts []WindowShift `json:"window_shifts"`
}

// WindowShift records one adaptive window change.
type WindowShift struct {
	AtMbps   float64 `json:"at_mbps"`
	From     int     `json:"from"`
	To       int     `json:"to"`
	Buffered int     `json:"buffered"`
}

// Receiver reorders one RTP stream.
type Receiver struct {
	cfg Config

	started    bool
	next       uint64
	maxSeen    uint64
	highestSeq uint16
	highestExt uint64
	buffer     map[uint64]*Packet
	lostSet    map[uint64]bool
	delivered  map[uint64]bool
	arrived    int
	window     int

	stats Stats
}

// NewReceiver creates a receiver for cfg, applying defaults for zero fields.
