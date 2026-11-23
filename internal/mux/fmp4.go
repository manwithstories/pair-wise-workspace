// Package mux 用 go-mp4 把无损整数 PCM 分段封装为 fMP4（ipcm 采样条目），
// 并通过回读 box 树与提取 mdat 载荷核验产物完整性与字节一致性。
package mux

import (
	"io"

	"github.com/abema/go-mp4"

	"media-pipeline/internal/clock"
	"media-pipeline/internal/fault"
)

// Muxer 持有共享时间基准，输出每段独立可解码的 fMP4。
type Muxer struct {
	tb      clock.Timebase
	trackID uint32
}

// New 构造封装器，trackID 通常为 1。
func New(tb clock.Timebase, trackID uint32) (*Muxer, error) {
	if err := tb.Valid(); err != nil {
		return nil, fault.New(fault.KindOther, "", -1, -1, false,
			"invalid timebase").WithOp("mux.new").Wrap(err)
	}
	if trackID == 0 {
		trackID = 1
	}
	return &Muxer{tb: tb, trackID: trackID}, nil
}

func (m *Muxer) start(w *mp4.Writer, t mp4.BoxType) error {
	_, err := w.StartBox(&mp4.BoxInfo{Type: t})
	return err
}

func (m *Muxer) end(w *mp4.Writer) error {
	_, err := w.EndBox()
	return err
}

// marshal 写入一个有固定结构的 box 载荷。
func marshal(w io.Writer, b mp4.IImmutableBox) error {
	_, err := mp4.Marshal(w, b, mp4.Context{})
	return err
}

// WriteInit 写 fMP4 初始化段：ftyp + moov（含 ipcm/pcmC 采样条目）。
func (m *Muxer) WriteInit(w io.WriteSeeker) error {
	ww := mp4.NewWriter(w)
	if err := m.writeFtyp(ww); err != nil {
		return wrapErr(err)
	}
	if err := m.writeMoov(ww); err != nil {
		return wrapErr(err)
	}
	return nil
}

func (m *Muxer) writeFtyp(w *mp4.Writer) error {
	if err := m.start(w, mp4.BoxTypeFtyp()); err != nil {
		return err
	}
	err := marshal(w, &mp4.Ftyp{
		MajorBrand: [4]byte{'i', 's', 'o', 'm'},
		MinorVersion: 0,
		CompatibleBrands: []mp4.CompatibleBrandElem{
			{CompatibleBrand: [4]byte{'i', 's', 'o', 'm'}},
			{CompatibleBrand: [4]byte{'i', 'p', 'c', 'm'}},
		},
	})
	if err != nil {
		return err
	}
	return m.end(w)
}

func (m *Muxer) writeMoov(w *mp4.Writer) error {
	if err := m.start(w, mp4.BoxTypeMoov()); err != nil {
		return err
	}
	if err := m.writeMvhd(w); err != nil {
		return err
	}
	if err := m.writeTrak(w); err != nil {
		return err
	}
	if err := m.writeMvex(w); err != nil {
		return err
	}
	return m.end(w)
}

func (m *Muxer) writeMvhd(w *mp4.Writer) error {
	if err := m.start(w, mp4.BoxTypeMvhd()); err != nil {
		return err
	}
	err := marshal(w, &mp4.Mvhd{
		FullBox:        mp4.FullBox{Version: 0},
		Timescale:      m.tb.SampleRate,
		DurationV0:     0,
		NextTrackID:    m.trackID + 1,
	})
	if err != nil {
		return err
	}
	return m.end(w)
}

func (m *Muxer) writeTrak(w *mp4.Writer) error {
	if err := m.start(w, mp4.BoxTypeTrak()); err != nil {
		return err
	}
	if err := m.writeTkhd(w); err != nil {
		return err
	}
	if err := m.writeMdia(w); err != nil {
		return err
	}
	return m.end(w)
}

func (m *Muxer) writeTkhd(w *mp4.Writer) error {
	if err := m.start(w, mp4.BoxTypeTkhd()); err != nil {
		return err
	}
	err := marshal(w, &mp4.Tkhd{
		FullBox:           mp4.FullBox{Version: 0, Flags: [3]byte{0x00, 0x00, 0x03}},
		TrackID:           m.trackID,
		DurationV0:        0,
		AlternateGroup:    0,
		Volume:            0x0100, // 音频轨 1.0 定点音量
	})
	if err != nil {
		return err
	}
	return m.end(w)
}

func (m *Muxer) writeMdia(w *mp4.Writer) error {
	if err := m.start(w, mp4.BoxTypeMdia()); err != nil {
		return err
	}
	if err := m.writeMdhd(w); err != nil {
		return err
	}
	if err := m.writeHdlr(w); err != nil {
		return err
	}
	if err := m.writeMinf(w); err != nil {
		return err
	}
	return m.end(w)
}

func (m *Muxer) writeMdhd(w *mp4.Writer) error {
	if err := m.start(w, mp4.BoxTypeMdhd()); err != nil {
		return err
	}
	err := marshal(w, &mp4.Mdhd{
		FullBox:     mp4.FullBox{Version: 0},
		Timescale:   m.tb.SampleRate,
		Language:    [3]byte{'u', 'n', 'd'},
	})
	if err != nil {
		return err
	}
	return m.end(w)
}

func (m *Muxer) writeHdlr(w *mp4.Writer) error {
	if err := m.start(w, mp4.BoxTypeHdlr()); err != nil {
		return err
	}
	err := marshal(w, &mp4.Hdlr{
		FullBox:    mp4.FullBox{Version: 0},
		HandlerType: [4]byte{'s', 'o', 'u', 'n'},
		Name:        "pcm-audio",
	})
	if err != nil {
		return err
	}
	return m.end(w)
}

func (m *Muxer) writeMinf(w *mp4.Writer) error {
	if err := m.start(w, mp4.BoxTypeMinf()); err != nil {
		return err
	}
	if err := m.start(w, mp4.BoxTypeSmhd()); err != nil {
		return err
	}
	if err := marshal(w, &mp4.Smhd{FullBox: mp4.FullBox{}}); err != nil {
		return err
	}
	if err := m.end(w); err != nil {
		return err
	}
	if err := m.writeDinf(w); err != nil {
		return err
	}
	if err := m.writeStbl(w); err != nil {
		return err
	}
	return m.end(w)
}

func (m *Muxer) writeDinf(w *mp4.Writer) error {
	if err := m.start(w, mp4.BoxTypeDinf()); err != nil {
		return err
	}
	if err := m.start(w, mp4.BoxTypeDref()); err != nil {
		return err
	}
	if err := marshal(w, &mp4.Dref{FullBox: mp4.FullBox{}, EntryCount: 1}); err != nil {
		return err
	}
	if err := m.start(w, mp4.BoxTypeUrl()); err != nil {
		return err
	}
	if err := marshal(w, &mp4.Url{FullBox: mp4.FullBox{Flags: [3]byte{0, 0, 1}}}); err != nil {
		return err
	}
	if err := m.end(w); err != nil { // url
		return err
	}
	if err := m.end(w); err != nil { // dref
		return err
	}
	return m.end(w) // dinf
}


func (m *Muxer) writeMvex(w *mp4.Writer) error {
	if err := m.start(w, mp4.BoxTypeMvex()); err != nil {
		return err
	}
	if err := m.start(w, mp4.BoxTypeTrex()); err != nil {
		return err
	}
	err := marshal(w, &mp4.Trex{
		FullBox:                      mp4.FullBox{},
		TrackID:                      m.trackID,
		DefaultSampleDescriptionIndex: 1,
		DefaultSampleDuration:         uint32(m.tb.FrameSize()),
	})
	if err != nil {
		return err
	}
	if err := m.end(w); err != nil { // trex
		return err
	}
	return m.end(w) // mvex
}
