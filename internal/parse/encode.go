package parse

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"

	"mediarun/internal/model"
)

// EncodeMovie serialises m into a deterministic byte slice.
func EncodeMovie(m *Movie) ([]byte, error) {
	if m.Timescale == 0 {
		return nil, fmt.Errorf("%w: movie timescale is zero", model.ErrInvalid)
	}
	if len(m.Tracks) == 0 {
		return nil, fmt.Errorf("%w: movie has no tracks", model.ErrInvalid)
	}
	ftyp := encodeFtyp()
	// First pass with zeroed chunk offsets gives moov's exact size, since
	// stco encodes one 32-bit entry per sample either way.
	sizeMoov, err := m.moov(sizeZero(m))
	if err != nil {
		return nil, err
	}
	mdatBody := m.layoutSize()
	if mdatBody+8 > math.MaxUint32 {
		return nil, fmt.Errorf("%w: mdat exceeds 32-bit box size", model.ErrLimit)
	}
	start := int64(len(ftyp)) + int64(len(sizeMoov)) + 8
	offsets, err := m.layout(start)
	if err != nil {
		return nil, err
	}
	moov, err := m.moov(offsets)
	if err != nil {
		return nil, err
	}
	if len(moov) != len(sizeMoov) {
		return nil, fmt.Errorf("parse: moov size changed between passes: %d vs %d", len(moov), len(sizeMoov))
	}
	var out bytes.Buffer
	out.Write(ftyp)
	out.Write(moov)
	var hdr [8]byte
	binary.BigEndian.PutUint32(hdr[0:4], uint32(8+mdatBody))
	copy(hdr[4:8], "mdat")
	out.Write(hdr[:])
	for _, t := range m.Tracks {
		for _, s := range t.Samples {
			out.Write(s.Data)
		}
	}
	for _, extra := range m.Extras {
		switch extra.String() {
		case "ftyp", "moov", "mdat":
			continue
		}
		out.Write(extra.Bytes())
	}
	return out.Bytes(), nil
}
