package testdata

import (
	"fmt"
	"io"

	mp4 "github.com/abema/go-mp4"
)

// memWS 是内存中的 io.WriteSeeker，供 go-mp4 Writer 回写 box 头。
type memWS struct {
	buf []byte
	off int64
}

func (m *memWS) Write(p []byte) (int, error) {
	end := m.off + int64(len(p))
	if end > int64(len(m.buf)) {
		nb := make([]byte, end)
		copy(nb, m.buf)
		m.buf = nb
	}
	copy(m.buf[m.off:], p)
	m.off = end
	return len(p), nil
}

func (m *memWS) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekCurrent:
		offset += m.off
	case io.SeekEnd:
		offset += int64(len(m.buf))
	}
	if offset < 0 {
		return 0, fmt.Errorf("负偏移 %d", offset)
	}
	m.off = offset
	return offset, nil
}

var identityMatrix = [9]int32{0x10000, 0, 0, 0, 0x10000, 0, 0, 0, 0x40000000}

// MP4 生成一条 AVC1 视频轨的本地 MP4：每帧一个样本，
// 样本时长 3600/90000s（25fps），样本大小取自 frameSizes。
// 结构与演示 RTP 流的媒体时间线对齐，供 manifest 读取验证。
func MP4(frameSizes []uint32) ([]byte, error) {
	ws := &memWS{}
	w := mp4.NewWriter(ws)
	ctx := mp4.Context{}
	n := uint32(len(frameSizes))
	const delta = 3600 // 25fps @ 90kHz

	box := func(t mp4.BoxType) {
		if _, err := w.StartBox(&mp4.BoxInfo{Type: t}); err != nil {
			panic(err)
		}
	}
	end := func() {
		if _, err := w.EndBox(); err != nil {
			panic(err)
		}
	}
	put := func(b mp4.IImmutableBox) {
		if _, err := mp4.Marshal(w, b, ctx); err != nil {
			panic(err)
		}
	}
	defer func() {
		if r := recover(); r != nil {
			// 生成失败属于程序错误，直接暴露
			panic(r)
		}
	}()

	box(mp4.BoxTypeFtyp())
	put(&mp4.Ftyp{
		MajorBrand:   mp4.BrandISOM(),
		MinorVersion: 0x200,
		CompatibleBrands: []mp4.CompatibleBrandElem{
			{CompatibleBrand: mp4.BrandISOM()},
			{CompatibleBrand: mp4.BrandAVC1()},
		},
	})
	end()

	mbi, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMdat()})
	if err != nil {
		return nil, err
	}
	mdatPayload := mbi.Offset + mbi.HeaderSize
	var total int64
	for _, s := range frameSizes {
		total += int64(s)
	}
	zeros := make([]byte, 65536)
	for total > 0 {
		chunk := total
		if chunk > int64(len(zeros)) {
			chunk = int64(len(zeros))
		}
		if _, err := w.Write(zeros[:chunk]); err != nil {
			return nil, err
		}
		total -= chunk
	}
	end()

	box(mp4.BoxTypeMoov())
	box(mp4.BoxTypeMvhd())
	put(&mp4.Mvhd{
		Timescale: 1000, DurationV0: n * FrameMs,
		Rate: 0x10000, Volume: 0x0100,
		Matrix: identityMatrix, NextTrackID: 2,
	})
	end()
	box(mp4.BoxTypeTrak())
	box(mp4.BoxTypeTkhd())
	put(&mp4.Tkhd{
		FullBox:    mp4.FullBox{Flags: [3]byte{0, 0, 7}},
		TrackID:    1,
		DurationV0: n * FrameMs,
		Matrix:     identityMatrix,
		Width:      1280 << 16,
		Height:     720 << 16,
	})
	end()
	box(mp4.BoxTypeMdia())
	box(mp4.BoxTypeMdhd())
	put(&mp4.Mdhd{
		Timescale:  ClockRate,
		DurationV0: n * delta,
		Language:   [3]byte{'u', 'n', 'd'},
	})
	end()
	box(mp4.BoxTypeMinf())
	box(mp4.BoxTypeStbl())
	box(mp4.BoxTypeStsd())
	put(&mp4.Stsd{EntryCount: 1})
	box(mp4.BoxTypeAvc1())
	vse := &mp4.VisualSampleEntry{
		SampleEntry:     mp4.SampleEntry{DataReferenceIndex: 1},
		Width:           1280,
		Height:          720,
		Horizresolution: 0x480000,
		Vertresolution:  0x480000,
		FrameCount:      1,
		Depth:           0x18,
		PreDefined3:     -1,
	}
	vse.SetType(mp4.BoxTypeAvc1()) // AnyTypeBox 需显式设置类型
	put(vse)
	box(mp4.BoxTypeAvcC())
	avcc := &mp4.AVCDecoderConfiguration{
		ConfigurationVersion:       1,
		Profile:                    100,
		ProfileCompatibility:       0,
		Level:                      40,
		LengthSizeMinusOne:         3,
		NumOfSequenceParameterSets: 1,
		SequenceParameterSets: []mp4.AVCParameterSet{
			{Length: 4, NALUnit: []byte{0x67, 0x64, 0x00, 0x28}},
		},
		NumOfPictureParameterSets: 1,
		PictureParameterSets: []mp4.AVCParameterSet{
			{Length: 4, NALUnit: []byte{0x68, 0xee, 0x3c, 0x80}},
		},
	}
	avcc.SetType(mp4.BoxTypeAvcC())
	put(avcc)
	end() // avcC
	end() // avc1
	end() // stsd
	box(mp4.BoxTypeStts())
	put(&mp4.Stts{
		EntryCount: 1,
		Entries:    []mp4.SttsEntry{{SampleCount: n, SampleDelta: delta}},
	})
	end()
	box(mp4.BoxTypeStsc())
	put(&mp4.Stsc{
		EntryCount: 1,
		Entries:    []mp4.StscEntry{{FirstChunk: 1, SamplesPerChunk: n, SampleDescriptionIndex: 1}},
	})
	end()
	box(mp4.BoxTypeStsz())
	put(&mp4.Stsz{SampleCount: n, EntrySize: frameSizes})
	end()
	box(mp4.BoxTypeStco())
	put(&mp4.Stco{EntryCount: 1, ChunkOffset: []uint32{uint32(mdatPayload)}})
	end()
	end() // stbl
	end() // minf
	end() // mdia
	end() // trak
	end() // moov
	return ws.buf, nil
}
