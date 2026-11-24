// Package parse reads and writes the ISO BMFF subset used by the delivery
// pipeline. Unknown boxes are preserved as raw bytes so a re-written file
// keeps every byte the pipeline does not understand.
package parse

// Limits bound memory use for hostile or corrupt inputs.
const (
	MaxBoxDepth   = 12
	MaxFileBytes  = 64 << 20
	MaxBoxes      = 4096
	MaxBoxPayload = 32 << 20
)

// RawBox is one box exactly as it appeared in the container.
type RawBox struct {
	Type       [4]byte
	Offset     int64
	Size       uint64 // total size including the header
	HeaderSize uint64
	Payload    []byte // bytes after the header; empty for container boxes
	Children   []RawBox
	Parent     string
}

// String renders the box type as text.
func (b RawBox) String() string { return string(b.Type[:]) }
