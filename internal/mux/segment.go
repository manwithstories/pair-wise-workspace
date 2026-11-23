package mux

import (
	"io"

	"github.com/abema/go-mp4"

	"media-pipeline/internal/fault"
)

// measureMoof 用占位 DataOffset 试写一次 moof，得到其确定字节大小。
// moof 各 box 均为定长字段，尺寸不随 DataOffset 取值变化。
func (m *Muxer) measureMoof(seq uint32, baseSample uint64, pcmLen int) (int, error) {
	var probe memSink
	if err := m.writeMoof(mp4.NewWriter(&probe), seq, baseSample, pcmLen, 0); err != nil {
		return 0, err
	}
	return probe.Len(), nil
}

// memSink 是只写的内存计数/收集器，用于试写测量 box 尺寸。
type memSink struct{ b []byte }

func (s *memSink) Write(p []byte) (int, error) { s.b = append(s.b, p...); return len(p), nil }
func (s *memSink) Seek(int64, int) (int64, error) { return int64(len(s.b)), nil }
func (s *memSink) Len() int                       { return len(s.b) }

// writeMoof 写入 moof>traf>(mfhd,tfhd,tfdt,trun)，dataOffset 由调用方给定。
func (m *Muxer) writeMoof(ww *mp4.Writer, seq uint32, baseSample uint64, pcmLen, dataOffset int) error {
	if err := m.start(ww, mp4.BoxTypeMoof()); err != nil {
		return err
	}
	if err := m.start(ww, mp4.BoxTypeTraf()); err != nil {
		return err
	}
	if err := m.start(ww, mp4.BoxTypeMfhd()); err != nil {
		return err
	}
	if err := marshal(ww, &mp4.Mfhd{FullBox: mp4.FullBox{}, SequenceNumber: seq}); err != nil {
		return err
	}
	if err := m.end(ww); err != nil {
		return err
	}

	if err := m.start(ww, mp4.BoxTypeTfhd()); err != nil {
		return err
	}
	if err := marshal(ww, &mp4.Tfhd{FullBox: mp4.FullBox{}, TrackID: m.trackID}); err != nil {
		return err
	}
	if err := m.end(ww); err != nil {
		return err
	}

	if err := m.start(ww, mp4.BoxTypeTfdt()); err != nil {
		return err
	}
	if err := marshal(ww, &mp4.Tfdt{
		FullBox:               mp4.FullBox{Version: 1},
		BaseMediaDecodeTimeV1: baseSample,
	}); err != nil {
		return err
	}
	if err := m.end(ww); err != nil {
		return err
	}

	if err := m.start(ww, mp4.BoxTypeTrun()); err != nil {
		return err
	}
	trun := &mp4.Trun{
		FullBox:     mp4.FullBox{Flags: [3]byte{0x00, 0x02, 0x01}}, // data-offset + sample-size
		SampleCount: 1,                                             // 整段 PCM 作为单个 sample
		DataOffset:  int32(dataOffset),
		Entries:     []mp4.TrunEntry{{SampleSize: uint32(pcmLen)}},
	}
	if err := marshal(ww, trun); err != nil {
		return err
	}
	if err := m.end(ww); err != nil { // trun
		return err
	}
	if err := m.end(ww); err != nil { // traf
		return err
	}
	return m.end(ww) // moof
}

func (m *Muxer) writeStyp(ww *mp4.Writer) error {
	if err := m.start(ww, mp4.BoxTypeStyp()); err != nil {
		return err
	}
	if err := marshal(ww, &mp4.Styp{
		MajorBrand: [4]byte{'i', 's', 'o', 'm'},
		CompatibleBrands: []mp4.CompatibleBrandElem{
			{CompatibleBrand: [4]byte{'i', 's', 'o', 'm'}},
		},
	}); err != nil {
		return err
	}
	return m.end(ww)
}

// WriteSegment 写一个媒体段：styp + moof(tfhd/tfdt/trun) + mdat(原始 PCM)。
// baseSample 是该段首帧的全局采样序号，用于 tfdt 时间线。
// 写入器需同时支持读，以便回读核验。
func (m *Muxer) WriteSegment(w io.ReadWriteSeeker, seq uint32, baseSample uint64, pcm []byte) error {
	fs := m.tb.FrameSize()
	if len(pcm)%fs != 0 {
		return fault.New(fault.KindMalformedInput, "", int64(len(pcm)-len(pcm)%fs), -1, true,
			"segment PCM is not frame aligned").WithOp("mux.segment")
	}
	if len(pcm) == 0 {
		return fault.New(fault.KindMalformedInput, "", 0, -1, true,
			"refusing to mux empty segment").WithOp("mux.segment")
	}

	moofSize, err := m.measureMoof(seq, baseSample, len(pcm))
	if err != nil {
		return wrapErr(err)
	}
	// 段布局：styp + moof + mdat(8 字节头 + PCM)。
	// trun.DataOffset 相对 moof 起点，需越过整个 moof 与 mdat 头部。
	dataOffset := moofSize + 8

	ww := mp4.NewWriter(w)
	if err := m.writeStyp(ww); err != nil {
		return wrapErr(err)
	}
	if err := m.writeMoof(ww, seq, baseSample, len(pcm), dataOffset); err != nil {
		return wrapErr(err)
	}
	if err := m.start(ww, mp4.BoxTypeMdat()); err != nil {
		return wrapErr(err)
	}
	if _, err := ww.Write(pcm); err != nil {
		return wrapErr(err)
	}
	if err := m.end(ww); err != nil {
		return wrapErr(err)
	}
	return nil
}
