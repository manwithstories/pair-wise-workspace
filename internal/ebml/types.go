package ebml

// Vint is one decoded variable length integer plus its encoded width.
type Vint struct {
	Value  uint64
	Width  int
	Offset int64
	// Unknown is true when the size payload is all ones (unknown-length
	// element: legal for Segment and Cluster).
	Unknown bool
}

// Element is a header (ID + size) plus its absolute body offset.
type Element struct {
	ID       uint64
	Size     int64
	Unknown  bool
	BodyOff  int64
	HeadOff  int64
	HeadSize int64
}
