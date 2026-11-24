package parse

import (
	"fmt"

	mp4 "github.com/abema/go-mp4"

	"mediarun/internal/clock"
	"mediarun/internal/model"
)

// moov builds the complete moov box.
func (m *Movie) moov(offsets [][]int64) ([]byte, error) {
	movieDur, err := m.durationInMovieScale()
	if err != nil {
		return nil, err
	}
	mvhdBody, err := marshalBody(&mp4.Mvhd{
		Timescale:   m.Timescale,
		DurationV0:  uint32(movieDur),
		Rate:        0x00010000,
		Volume:      0x0100,
		Matrix:      identityMatrix(),
		NextTrackID: uint32(len(m.Tracks) + 1),
	})
	if err != nil {
		return nil, err
	}
	children := []*box{leaf("mvhd", mvhdBody)}
	for i, t := range m.Tracks {
		var off []int64
		if i < len(offsets) {
			off = offsets[i]
		}
		trak, err := m.trak(t, off)
		if err != nil {
			return nil, err
		}
		children = append(children, trak)
	}
	if len(m.Udta) > 0 {
		var udta []*box
		for _, u := range m.Udta {
			udta = append(udta, rawToBox(u))
		}
		children = append(children, container("udta", udta...))
	}
	return container("moov", children...).encode(), nil
}

// rawToBox converts a preserved raw box into the writer tree. Leaves keep
// their exact payload bytes.
func rawToBox(r RawBox) *box {
	b := &box{typ: r.Type, payload: r.Payload}
	if len(r.Children) > 0 {
		for _, c := range r.Children {
			b.children = append(b.children, rawToBox(c))
		}
	}
	return b
}

// durationInMovieScale converts the longest track duration into movie units.
func (m *Movie) durationInMovieScale() (uint64, error) {
	best := m.Duration
	for _, t := range m.Tracks {
		var total uint64
		for _, s := range t.Samples {
			total += uint64(s.Duration)
		}
		ticks, err := clock.ScaleToTicks(int64(total), t.Timescale)
		if err != nil {
			return 0, err
		}
		units, err := clock.TicksToUnits(ticks, m.Timescale)
		if err != nil {
			return 0, err
		}
		if units < 0 {
			return 0, fmt.Errorf("%w: negative track duration", model.ErrInvalid)
		}
		if uint64(units) > best {
			best = uint64(units)
		}
	}
	return best, nil
}

func (m *Movie) trak(t Track, offsets []int64) (*box, error) {
	var total uint64
	for _, s := range t.Samples {
		total += uint64(s.Duration)
	}
	ticks, err := clock.ScaleToTicks(int64(total), t.Timescale)
	if err != nil {
		return nil, err
	}
	trackDur, err := clock.TicksToUnits(ticks, m.Timescale)
	if err != nil {
		return nil, err
	}
	if trackDur < 0 {
		return nil, fmt.Errorf("%w: negative track duration", model.ErrInvalid)
	}
	tkhdBody, err := marshalBody(&mp4.Tkhd{
		TrackID:    t.ID,
		DurationV0: uint32(trackDur),
		Matrix:     identityMatrix(),
		Width:      uint32(t.Width) << 16,
		Height:     uint32(t.Height) << 16,
	})
	if err != nil {
		return nil, err
	}
	mdhdBody, err := marshalBody(&mp4.Mdhd{
		Timescale:  t.Timescale,
		DurationV0: uint32(total),
		Language:   encodeLanguage(t.Language),
	})
	if err != nil {
		return nil, err
	}
	handler := t.Handler
	if handler == "" {
		handler = "undf"
	}
	hdlrBody, err := marshalBody(&mp4.Hdlr{
		HandlerType: [4]byte{handler[0], handler[1], handler[2], handler[3]},
		Name:        handlerName(handler),
	})
	if err != nil {
		return nil, err
	}
	minf, err := writeMinf(t, offsets)
	if err != nil {
		return nil, err
	}
	mdia := container("mdia",
		leaf("mdhd", mdhdBody),
		leaf("hdlr", hdlrBody),
		minf,
	)
	return container("trak", leaf("tkhd", tkhdBody), mdia), nil
}

func writeMinf(t Track, offsets []int64) (*box, error) {
	var mediaHeader *box
	switch t.Handler {
	case "vide":
		body, err := marshalBody(&mp4.Vmhd{})
		if err != nil {
			return nil, err
		}
		mediaHeader = leaf("vmhd", body)
	case "soun":
		body, err := marshalBody(&mp4.Smhd{})
		if err != nil {
			return nil, err
		}
		mediaHeader = leaf("smhd", body)
	default:
		// nmhd is a FullBox with no additional fields.
		mediaHeader = leaf("nmhd", []byte{0, 0, 0, 0})
	}
	drefBody, err := marshalBody(&mp4.Dref{EntryCount: 1})
	if err != nil {
		return nil, err
	}
	urlBody, err := marshalBody(&mp4.Url{})
	if err != nil {
		return nil, err
	}
	dinf := container("dinf", payloadBox("dref", drefBody).withChildren(leaf("url ", urlBody)))
	stbl, err := writeStbl(t, offsets)
	if err != nil {
		return nil, err
	}
	return container("minf", mediaHeader, dinf, stbl), nil
}
