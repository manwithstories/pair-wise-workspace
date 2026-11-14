package fixture

import (
	"bytes"

	mp4 "github.com/abema/go-mp4"
)

// marshalPayload serializes a go-mp4 leaf box body (version/flags + fields).
func marshalPayload(b mp4.IImmutableBox) ([]byte, error) {
	var buf bytes.Buffer
	if _, err := mp4.Marshal(&buf, b, mp4.Context{}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func leaf(typ string, b mp4.IImmutableBox) ([]byte, error) {
	p, err := marshalPayload(b)
	if err != nil {
		return nil, err
	}
	return box(typ, p), nil
}

func mvhdBox(ts uint32, duration uint64) ([]byte, error) {
	m := &mp4.Mvhd{
		Timescale:   ts,
		DurationV1:  duration,
		Rate:        0x00010000,
		Volume:      0x0100,
		Matrix:      [9]int32{0x00010000, 0, 0, 0, 0x00010000, 0, 0, 0, 0x40000000},
		NextTrackID: 3,
	}
	m.SetVersion(1)
	return leaf("mvhd", m)
}

func tkhdBox(id uint32, dur uint64, vide bool) ([]byte, error) {
	t := &mp4.Tkhd{
		TrackID:    id,
		DurationV1: dur,
		Volume:     0,
		Matrix:     [9]int32{0x00010000, 0, 0, 0, 0x00010000, 0, 0, 0, 0x40000000},
		Width:      1920 << 16,
		Height:     1080 << 16,
	}
	t.SetVersion(1)
	if !vide {
		t.Width, t.Height = 0, 0
		t.Volume = 0x0100
	}
	return leaf("tkhd", t)
}

func mdhdBox(ts uint32, dur uint64) ([]byte, error) {
	m := &mp4.Mdhd{
		Timescale:  ts,
		DurationV1: dur,
		Language:   [3]byte{0x55, 0xC4, 0x00}, // packed ISO-639 "und"
	}
	m.SetVersion(1)
	return leaf("mdhd", m)
}

func hdlrBox(handler string) ([]byte, error) {
	h := &mp4.Hdlr{HandlerType: to4(handler), Name: handler + " handler"}
	return leaf("hdlr", h)
}

func to4(s string) [4]byte {
	var x [4]byte
	copy(x[:], s)
	return x
}

// sttsRunBox merges identical consecutive deltas into run entries.
func sttsRunBox(deltas []uint32) ([]byte, error) {
	s := &mp4.Stts{}
	for i := 0; i < len(deltas); {
		j := i + 1
		for j < len(deltas) && deltas[j] == deltas[i] {
			j++
		}
		s.Entries = append(s.Entries, mp4.SttsEntry{
			SampleCount: uint32(j - i), SampleDelta: deltas[i],
		})
		i = j
	}
	s.EntryCount = uint32(len(s.Entries))
	return leaf("stts", s)
}

func cttsBoxV1(off []int64) ([]byte, error) {
	c := &mp4.Ctts{}
	c.SetVersion(1)
	for _, o := range off {
		c.Entries = append(c.Entries, mp4.CttsEntry{
			SampleCount: 1, SampleOffsetV1: int32(o),
		})
	}
	c.EntryCount = uint32(len(c.Entries))
	return leaf("ctts", c)
}

// cttsBoxV0 writes unsigned composition offsets (version 0).
func cttsBoxV0(off []int64) ([]byte, error) {
	c := &mp4.Ctts{} // version 0 is the default
	for _, o := range off {
		if o < 0 {
			o = 0 // v0 cannot represent negative offsets
		}
		c.Entries = append(c.Entries, mp4.CttsEntry{
			SampleCount: 1, SampleOffsetV0: uint32(o),
		})
	}
	c.EntryCount = uint32(len(c.Entries))
	return leaf("ctts", c)
}

func stssBox(sync []bool) ([]byte, error) {
	s := &mp4.Stss{}
	for i, ok := range sync {
		if ok {
			s.SampleNumber = append(s.SampleNumber, uint32(i+1))
		}
	}
	s.EntryCount = uint32(len(s.SampleNumber))
	return leaf("stss", s)
}

func stscBox(perChunk int) ([]byte, error) {
	s := &mp4.Stsc{
		EntryCount: 1,
		Entries: []mp4.StscEntry{{
			FirstChunk: 1, SamplesPerChunk: uint32(perChunk),
			SampleDescriptionIndex: 1,
		}},
	}
	return leaf("stsc", s)
}

func stszBox(sizes []uint32) ([]byte, error) {
	s := &mp4.Stsz{SampleCount: uint32(len(sizes)), EntrySize: sizes}
	return leaf("stsz", s)
}

func stcoBox(off []int64) ([]byte, error) {
	c := &mp4.Stco{EntryCount: uint32(len(off))}
	for _, o := range off {
		c.ChunkOffset = append(c.ChunkOffset, uint32(o))
	}
	return leaf("stco", c)
}

func co64Box(off []int64) ([]byte, error) {
	c := &mp4.Co64{EntryCount: uint32(len(off))}
	for _, o := range off {
		c.ChunkOffset = append(c.ChunkOffset, uint64(o))
	}
	return leaf("co64", c)
}
