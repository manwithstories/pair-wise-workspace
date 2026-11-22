package fixture

import (
	"fmt"
	"io"

	"github.com/abema/go-mp4"

	"mediaroute/internal/clock"
)

// memWS is an in-memory io.WriteSeeker for building the init segment.
type memWS struct {
	buf []byte
	pos int64
}

func (m *memWS) Write(p []byte) (int, error) {
	end := m.pos + int64(len(p))
	if end > int64(len(m.buf)) {
		grown := make([]byte, end)
		copy(grown, m.buf)
		m.buf = grown
	}
	copy(m.buf[m.pos:], p)
	m.pos = end
	return len(p), nil
}

func (m *memWS) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = m.pos + offset
	case io.SeekEnd:
		abs = int64(len(m.buf)) + offset
	}
	if abs < 0 {
		return 0, fmt.Errorf("fixture: negative seek")
	}
	m.pos = abs
	return abs, nil
}

// buildInit writes a minimal init segment (ftyp + moov/mdhd) whose mdhd
// timescale is the unified 90 kHz clock.
func buildInit() ([]byte, error) {
	ws := &memWS{}
	w := mp4.NewWriter(ws)
	box := func(t mp4.BoxType, payload mp4.IImmutableBox) error {
		if _, err := w.StartBox(&mp4.BoxInfo{Type: t}); err != nil {
			return err
		}
		if _, err := mp4.Marshal(w, payload, mp4.Context{}); err != nil {
			return err
		}
		_, err := w.EndBox()
		return err
	}
	if err := box(mp4.BoxTypeFtyp(), &mp4.Ftyp{
		MajorBrand:   [4]byte{'i', 's', 'o', 'm'},
		MinorVersion: 1,
		CompatibleBrands: []mp4.CompatibleBrandElem{
			{CompatibleBrand: [4]byte{'i', 's', 'o', 'm'}},
		},
	}); err != nil {
		return nil, err
	}
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMoov()}); err != nil {
		return nil, err
	}
	if err := box(mp4.BoxTypeMdhd(), &mp4.Mdhd{
		Timescale:  clock.VideoClockHz,
		DurationV0: SegmentCount * SegmentDurationTicks,
		Language:   [3]byte{'u', 'n', 'd'},
	}); err != nil {
		return nil, err
	}
	if _, err := w.EndBox(); err != nil {
		return nil, err
	}
	return ws.buf, nil
}
