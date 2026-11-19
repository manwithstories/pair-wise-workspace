package mp4

import (
	"io"

	"github.com/abema/go-mp4"
)

// writeMinf writes the media information box: media header, data
// reference and an (empty) sample table with one stsd entry.
func (m *Muxer) writeMinf(t MuxTrack) func() error {
	return func() error {
		var err error
		switch t.Type {
		case "video":
			vm := &mp4.Vmhd{}
			vm.Flags[2] = 1
			err = m.box(mp4.BoxTypeVmhd(), vm)
		case "audio":
			err = m.box(mp4.BoxTypeSmhd(), &mp4.Smhd{})
		default:
			err = m.box(mp4.StrToBoxType("nmhd"), &Nmhd{})
		}
		if err != nil {
			return err
		}
		if err := m.with(mp4.BoxTypeDinf(), func() error {
			return m.with(mp4.BoxTypeDref(), func() error {
				if _, err := mp4.Marshal(m.w, &mp4.Dref{EntryCount: 1}, mp4.Context{}); err != nil {
					return err
				}
				u := &mp4.Url{}
				u.Flags[2] = 1 // self-contained
				return m.box(mp4.BoxTypeUrl(), u)
			})
		}); err != nil {
			return err
		}
		return m.with(mp4.BoxTypeStbl(), func() error {
			if err := m.writeStsd(t); err != nil {
				return err
			}
			for _, b := range []struct {
				t mp4.BoxType
				p mp4.IImmutableBox
			}{
				{mp4.BoxTypeStts(), &mp4.Stts{}},
				{mp4.BoxTypeStsc(), &mp4.Stsc{}},
				{mp4.BoxTypeStsz(), &mp4.Stsz{}},
				{mp4.BoxTypeStco(), &mp4.Stco{}},
			} {
				if err := m.box(b.t, b.p); err != nil {
					return err
				}
			}
			return nil
		})
	}
}

func (m *Muxer) writeStsd(t MuxTrack) error {
	return m.with(mp4.BoxTypeStsd(), func() error {
		if _, err := mp4.Marshal(m.w, &mp4.Stsd{EntryCount: 1}, mp4.Context{}); err != nil {
			return err
		}
		switch t.Codec {
		case "avc1":
			e := &mp4.VisualSampleEntry{
				SampleEntry:     mp4.SampleEntry{DataReferenceIndex: 1},
				Width:           t.Width,
				Height:          t.Height,
				Horizresolution: 0x480000, Vertresolution: 0x480000,
				FrameCount: 1, Depth: 0x18, PreDefined3: -1,
			}
			e.SetType(mp4.BoxTypeAvc1())
			return m.box(mp4.BoxTypeAvc1(), e)
		case "mp4a":
			e := &mp4.AudioSampleEntry{
				SampleEntry:  mp4.SampleEntry{DataReferenceIndex: 1},
				ChannelCount: 2, SampleSize: 16,
				SampleRate: t.Timescale << 16,
			}
			e.SetType(mp4.BoxTypeMp4a())
			return m.box(mp4.BoxTypeMp4a(), e)
		default: // wvtt and anything else: opaque subtitle entry
			e := &WvttSampleEntry{
				SampleEntry: mp4.SampleEntry{DataReferenceIndex: 1},
			}
			e.SetType(BoxTypeWvtt())
			return m.box(BoxTypeWvtt(), e)
		}
	})
}

// AddFragment appends one moof+mdat containing frags[i] for tracks[i].
// Sample payloads are copied verbatim (lossless passthrough). Samples of
// one track within a fragment must have contiguous DTS.
func (m *Muxer) AddFragment(frags []FragTrack) error {
	scratch := &memWS{}
	if err := m.marshalMoof(scratch, frags, 0); err != nil {
		return err
	}
	dataBase := int64(len(scratch.buf)) + 8 // moof size + mdat header
	if err := m.marshalMoof(m.out, frags, dataBase); err != nil {
		return err
	}
	if _, err := m.w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMdat()}); err != nil {
		return err
	}
	for _, f := range frags {
		for _, s := range f.Samples {
			if _, err := m.w.Write(s.Data); err != nil {
				return err
			}
		}
	}
	_, err := m.w.EndBox()
	m.seq++
	return err
}

// marshalMoof writes the moof for one fragment. dataBase is the offset
// from moof start to the first sample byte (0 while size-probing).
func (m *Muxer) marshalMoof(out io.WriteSeeker, frags []FragTrack, dataBase int64) error {
	w := mp4.NewWriter(out)
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMoof()}); err != nil {
		return err
	}
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMfhd()}); err != nil {
		return err
	}
	if _, err := mp4.Marshal(w, &mp4.Mfhd{SequenceNumber: m.seq}, mp4.Context{}); err != nil {
		return err
	}
	if _, err := w.EndBox(); err != nil {
		return err
	}
	payloadOff := int64(0) // offset of this track's payload within mdat
	for i, f := range frags {
		if len(f.Samples) == 0 {
			continue
		}
		if err := m.marshalTraf(w, m.tracks[i], f, dataBase+payloadOff); err != nil {
			return err
		}
		for _, s := range f.Samples {
			payloadOff += int64(len(s.Data))
		}
	}
	_, err := w.EndBox()
	return err
}

func (m *Muxer) marshalTraf(w *mp4.Writer, t MuxTrack, f FragTrack, dataOffset int64) error {
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeTraf()}); err != nil {
		return err
	}
	tfhd := &mp4.Tfhd{TrackID: t.ID}
	tfhd.Flags[1] = 0x02 // default-base-is-moof
	if err := marshalBox(w, mp4.BoxTypeTfhd(), tfhd); err != nil {
		return err
	}
	var tfdt mp4.IImmutableBox
	if m.Tfdt32 {
		b := &mp4.Tfdt{BaseMediaDecodeTimeV0: uint32(f.Samples[0].DTS)}
		tfdt = b
	} else {
		b := &mp4.Tfdt{BaseMediaDecodeTimeV1: uint64(f.Samples[0].DTS)}
		b.Version = 1
		tfdt = b
	}
	if err := marshalBox(w, mp4.BoxTypeTfdt(), tfdt); err != nil {
		return err
	}
	trun := &mp4.Trun{
		SampleCount: uint32(len(f.Samples)),
		DataOffset:  int32(dataOffset),
	}
	trun.Version = 1 // signed composition offsets
	trun.Flags[1], trun.Flags[2] = 0x0f, 0x01 // data-offset|duration|size|flags|cto
	for _, s := range f.Samples {
		var flags uint32 = 0x02000000 // sample_depends_on=2 (sync)
		if !s.Keyframe {
			flags = 0x00010000 // sample_is_non_sync_sample
		}
		trun.Entries = append(trun.Entries, mp4.TrunEntry{
			SampleDuration:                s.Duration,
			SampleSize:                    uint32(len(s.Data)),
			SampleFlags:                   flags,
			SampleCompositionTimeOffsetV1: s.CTSOffset,
		})
	}
	if err := marshalBox(w, mp4.BoxTypeTrun(), trun); err != nil {
		return err
	}
	_, err := w.EndBox() // traf
	return err
}

func marshalBox(w *mp4.Writer, t mp4.BoxType, p mp4.IImmutableBox) error {
	if _, err := w.StartBox(&mp4.BoxInfo{Type: t}); err != nil {
		return err
	}
	if _, err := mp4.Marshal(w, p, mp4.Context{}); err != nil {
		return err
	}
	_, err := w.EndBox()
	return err
}

// memWS is an in-memory io.WriteSeeker used to size boxes before writing.
type memWS struct {
	buf []byte
	pos int64
}

func (m *memWS) Write(p []byte) (int, error) {
	end := m.pos + int64(len(p))
	if end > int64(len(m.buf)) {
		buf := make([]byte, end)
		copy(buf, m.buf)
		m.buf = buf
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
	m.pos = abs
	return abs, nil
}
