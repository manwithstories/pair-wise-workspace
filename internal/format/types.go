package format

import (
	"encoding/binary"

	"medialab/internal/model"
)

// MaxContainerBytes bounds accepted MP4/MXF input. The local fixture set is
// tiny; the generous cap still prevents unbounded reads.
const MaxContainerBytes = 16 << 20

// Kind identifies a track's media type.
type Kind string

const (
	KindVideo Kind = "video"
	KindAudio Kind = "audio"
	KindData  Kind = "data"
)

// ColorDescription is one candidate color description. Source is retained for
// conflict adjudication and for diagnostics.
type ColorDescription struct {
	Primaries string
	Transfer  string
	Matrix    string
	Source    string
	Offset    int64
}

// Crop stores inclusive/exclusive? Here margins in decoded pixels: columns
// removed from left/right and rows removed from top/bottom.
type Crop struct {
	Left   int64
	Right  int64
	Top    int64
	Bottom int64
	Offset int64
}

// Track is the implementation-supported metadata subset for one track.
type Track struct {
	ID         uint32
	Kind       Kind
	Handler    string
	Timescale  uint32
	Duration   uint64
	Width      uint32
	Height     uint32
	Rotation   int
	RotationAt int64
	Crop       *Crop
	Color      *ColorDescription
	Keyframes  []uint32
	// DataOffset is the raw container offset at which this track began.
	DataOffset int64
}

// Container is the common result returned by MP4 and MXF parsers.
type Container struct {
	Format         string
	Tracks         []*Track
	ContainerColor *ColorDescription
	Raw            []byte
}

// boundedReader reads big-endian fields while converting every truncation
// into a Fault carrying an absolute raw byte offset.
type boundedReader struct {
	data  []byte
	base  int64
	pos   int
	track uint32
	stage model.Stage
}

func newBoundedReader(data []byte, base int64, stage model.Stage, track uint32) *boundedReader {
	return &boundedReader{data: data, base: base, stage: stage, track: track}
}

func (r *boundedReader) absolute() int64 { return r.base + int64(r.pos) }

func (r *boundedReader) fault(code string, off int64, err error) error {
	return model.New(r.stage, code, r.track, off, err)
}

func (r *boundedReader) need(n int, code string) error {
	if n < 0 || r.pos+n > len(r.data) {
		return r.fault("truncated_field", r.absolute(), errTruncated)
	}
	return nil
}

func (r *boundedReader) bytes(n int, code string) ([]byte, error) {
	if err := r.need(n, code); err != nil {
		return nil, err
	}
	b := r.data[r.pos : r.pos+n]
	r.pos += n
	return b, nil
}

func (r *boundedReader) u8(code string) (byte, error) {
	b, err := r.bytes(1, code)
	if err != nil {
		return 0, err
	}
	return b[0], nil
}

func (r *boundedReader) u16(code string) (uint16, error) {
	b, err := r.bytes(2, code)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint16(b), nil
}

func (r *boundedReader) u32(code string) (uint32, error) {
	b, err := r.bytes(4, code)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint32(b), nil
}

func (r *boundedReader) u64(code string) (uint64, error) {
	b, err := r.bytes(8, code)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint64(b), nil
}

func (r *boundedReader) skip(n int, code string) error {
	_, err := r.bytes(n, code)
	return err
}
