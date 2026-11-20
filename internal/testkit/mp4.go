package testkit

import (
	"bytes"
	"io"

	mp4 "github.com/abema/go-mp4"
)

// memWS 为内存 WriteSeeker，供 go-mp4 Writer 使用。
type memWS struct {
	buf []byte
	pos int64
}

func (m *memWS) Write(p []byte) (int, error) {
	end := m.pos + int64(len(p))
	if end > int64(len(m.buf)) {
		nb := make([]byte, end)
		copy(nb, m.buf)
		m.buf = nb
	}
	copy(m.buf[m.pos:], p)
	m.pos = end
	return len(p), nil
}

func (m *memWS) Seek(off int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		m.pos = off
	case io.SeekCurrent:
		m.pos += off
	case io.SeekEnd:
		m.pos = int64(len(m.buf)) + off
	}
	return m.pos, nil
}

// BuildMP4 构造最小 MP4：ftyp + moov(trak(mdia(mdhd, minf(stbl(stts,stsz,stco))))) + mdat。
// stco 为 chunk 偏移；good 样例用 FixupStco 回填真实 mdat 负载偏移。
func BuildMP4(timescale, sttsTotal, stszCount uint32, stszSizes []uint32, stco uint32, mdatSize int) ([]byte, error) {
	m := &memWS{}
	w := mp4.NewWriter(m)
	ctx := mp4.Context{}
	put := func(t mp4.BoxType, box mp4.IImmutableBox) error {
		if _, err := w.StartBox(&mp4.BoxInfo{Type: t}); err != nil {
			return err
		}
		if _, err := mp4.Marshal(w, box, ctx); err != nil {
			return err
		}
		_, err := w.EndBox()
		return err
	}
	if err := put(mp4.BoxTypeFtyp(), &mp4.Ftyp{MajorBrand: [4]byte{'i', 's', 'o', 'm'}, MinorVersion: 1}); err != nil {
		return nil, err
	}
	containers := []mp4.BoxType{mp4.BoxTypeMoov(), mp4.BoxTypeTrak(), mp4.BoxTypeMdia(), mp4.BoxTypeMinf(), mp4.BoxTypeStbl()}
	for _, c := range containers {
		if _, err := w.StartBox(&mp4.BoxInfo{Type: c}); err != nil {
			return nil, err
		}
	}
	if err := put(mp4.BoxTypeMdhd(), &mp4.Mdhd{Timescale: timescale, DurationV0: 90000}); err != nil {
		return nil, err
	}
	if err := put(mp4.BoxTypeStts(), &mp4.Stts{EntryCount: 1,
		Entries: []mp4.SttsEntry{{SampleCount: sttsTotal, SampleDelta: 1000}}}); err != nil {
		return nil, err
	}
	if err := put(mp4.BoxTypeStsz(), &mp4.Stsz{SampleCount: stszCount, EntrySize: stszSizes}); err != nil {
		return nil, err
	}
	if err := put(mp4.BoxTypeStco(), &mp4.Stco{EntryCount: 1, ChunkOffset: []uint32{stco}}); err != nil {
		return nil, err
	}
	for range containers {
		if _, err := w.EndBox(); err != nil {
			return nil, err
		}
	}
	if err := put(mp4.BoxTypeMdat(), &mp4.Mdat{Data: make([]byte, mdatSize)}); err != nil {
		return nil, err
	}
	return m.buf, nil
}

// BuildGoodMP4 两遍构造：先占位 stco=0，再回填真实 mdat 负载偏移。
func BuildGoodMP4(timescale, samples uint32, sizes []uint32, mdatSize int) ([]byte, error) {
	draft, err := BuildMP4(timescale, samples, samples, sizes, 0, mdatSize)
	if err != nil {
		return nil, err
	}
	idx := bytes.Index(draft, []byte("mdat"))
	if idx < 0 {
		return nil, io.ErrUnexpectedEOF
	}
	return BuildMP4(timescale, samples, samples, sizes, uint32(idx+4), mdatSize)
}
