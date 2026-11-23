package mux

import (
	"github.com/abema/go-mp4"
)

func (m *Muxer) writeStbl(w *mp4.Writer) error {
	if err := m.start(w, mp4.BoxTypeStbl()); err != nil {
		return err
	}
	if err := m.writeStsd(w); err != nil {
		return err
	}
	for _, b := range []func(*mp4.Writer) error{
		m.writeStts, m.writeStsc, m.writeStsz, m.writeStco,
	} {
		if err := b(w); err != nil {
			return err
		}
	}
	return m.end(w)
}

func (m *Muxer) writeStsd(w *mp4.Writer) error {
	if err := m.start(w, mp4.BoxTypeStsd()); err != nil {
		return err
	}
	if err := marshal(w, &mp4.Stsd{FullBox: mp4.FullBox{}, EntryCount: 1}); err != nil {
		return err
	}
	// ipcm 音频采样条目（ISO 23001-5 无损整数 PCM），子盒 pcmC 给定位深。
	if err := m.start(w, mp4.BoxTypeIpcm()); err != nil {
		return err
	}
	rate := uint32(m.tb.SampleRate) << 16 // 16.16 定点
	ase := &mp4.AudioSampleEntry{
		SampleEntry: mp4.SampleEntry{
			AnyTypeBox:         mp4.AnyTypeBox{Type: mp4.BoxTypeIpcm()},
			DataReferenceIndex: 1,
		},
		ChannelCount: m.tb.Channels,
		SampleSize:   m.tb.BitsPerSample,
		SampleRate:   rate,
	}
	if err := marshal(w, ase); err != nil {
		return err
	}
	if err := m.start(w, mp4.BoxTypePcmC()); err != nil {
		return err
	}
	if err := marshal(w, &mp4.PcmC{
		FullBox:       mp4.FullBox{},
		FormatFlags:   0,
		PCMSampleSize: uint8(m.tb.BitsPerSample),
	}); err != nil {
		return err
	}
	if err := m.end(w); err != nil { // pcmC
		return err
	}
	if err := m.end(w); err != nil { // ipcm
		return err
	}
	return m.end(w) // stsd
}

func (m *Muxer) emptyTable(w *mp4.Writer, t mp4.BoxType, b mp4.IImmutableBox) error {
	if err := m.start(w, t); err != nil {
		return err
	}
	if err := marshal(w, b); err != nil {
		return err
	}
	return m.end(w)
}

func (m *Muxer) writeStts(w *mp4.Writer) error {
	return m.emptyTable(w, mp4.BoxTypeStts(), &mp4.Stts{FullBox: mp4.FullBox{}})
}
func (m *Muxer) writeStsc(w *mp4.Writer) error {
	return m.emptyTable(w, mp4.BoxTypeStsc(), &mp4.Stsc{FullBox: mp4.FullBox{}})
}
func (m *Muxer) writeStsz(w *mp4.Writer) error {
	return m.emptyTable(w, mp4.BoxTypeStsz(), &mp4.Stsz{FullBox: mp4.FullBox{}})
}
func (m *Muxer) writeStco(w *mp4.Writer) error {
	return m.emptyTable(w, mp4.BoxTypeStco(), &mp4.Stco{FullBox: mp4.FullBox{}})
}
