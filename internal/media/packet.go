package media

// Packet is the common access-unit/packet representation exchanged by source,
// transport, cut, fingerprint and encode packages. Sequence is the transport
// sequence; container timestamps remain in TimeBase and are never rewritten by
// the transport layer.
type Packet struct {
	Sequence     uint64 `json:"sequence"`
	StreamIndex  int    `json:"stream_index"`
	PTS          int64  `json:"pts"`
	DTS          int64  `json:"dts"`
	TimeBase     Rat    `json:"time_base"`
	FileOffset   int64  `json:"file_offset"`
	Size         int    `json:"size"`
	Key          bool   `json:"key"`
	Recovered    bool   `json:"recovered,omitempty"`
	DecodeErrors int    `json:"decode_errors,omitempty"`
	Payload      []byte `json:"-"`
}

func (p Packet) PTSSec() float64 { return p.TimeBase.Seconds(p.PTS) }
func (p Packet) DTSSec() float64 { return p.TimeBase.Seconds(p.DTS) }

// Stamp identifies a stream position for timestamp diagnostics.
type Stamp struct {
	Stream   int
	PTS      int64
	DTS      int64
	Position int64
}
