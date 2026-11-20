package fixture

import (
	"os"

	mp4 "github.com/abema/go-mp4"

	"mediarepair/internal/fault"
)

// ftyp 写出文件类型盒。
func (mw *mp4Writer) ftyp() error {
	if err := mw.start(mp4.BoxTypeFtyp()); err != nil {
		return err
	}
	ftyp := &mp4.Ftyp{MajorBrand: mp4.BrandISO5(), MinorVersion: 0x200}
	ftyp.AddCompatibleBrand(mp4.BrandISO5())
	ftyp.AddCompatibleBrand(mp4.BrandMP41())
	if err := marshalBox(mw.w, ftyp); err != nil {
		return err
	}
	return mw.end()
}

// moov 写出双轨 moov。
func (mw *mp4Writer) moov(spec Spec, lay *layout) error {
	if err := mw.start(mp4.BoxTypeMoov()); err != nil {
		return err
	}
	if err := mw.start(mp4.BoxTypeMvhd()); err != nil {
		return err
	}
	mvhd := &mp4.Mvhd{Timescale: 1000, DurationV0: 0, Rate: 0x10000,
		Matrix: [9]int32{0x10000, 0, 0, 0, 0x10000, 0, 0, 0, 0x40000000}, NextTrackID: 3}
	mvhd.DurationV0 = uint32(len(lay.video) * 33)
	if err := marshalBox(mw.w, mvhd); err != nil {
		return err
	}
	if err := mw.end(); err != nil {
		return err
	}
	if err := mw.trak(trakVideo, spec, lay); err != nil {
		return err
	}
	if len(lay.audio) > 0 {
		if err := mw.trak(trakAudio, spec, lay); err != nil {
			return err
		}
	}
	return mw.end()
}

type trakKind int

const (
	trakVideo trakKind = iota
	trakAudio
)

// trak 写出一条轨道。
func (mw *mp4Writer) trak(kind trakKind, spec Spec, lay *layout) error {
	var trackID uint32
	var kindName [4]byte
	var handler string
	var timescale uint32
	var samples []samplePlan
	if kind == trakVideo {
		trackID, kindName, handler, timescale, samples = 1, [4]byte{'v', 'i', 'd', 'e'}, "VideoHandler", spec.VideoTimescale, lay.video
	} else {
		trackID, kindName, handler, timescale, samples = 2, [4]byte{'s', 'o', 'u', 'n'}, "SoundHandler", spec.AudioTimescale, lay.audio
	}
	if err := mw.start(mp4.BoxTypeTrak()); err != nil {
		return err
	}
	if err := mw.start(mp4.BoxTypeTkhd()); err != nil {
		return err
	}
	tkhd := &mp4.Tkhd{TrackID: trackID, DurationV0: uint32(len(samples) * 1000),
		Matrix: [9]int32{0x10000, 0, 0, 0, 0x10000, 0, 0, 0, 0x40000000}}
	if kind == trakVideo {
		tkhd.Width, tkhd.Height = 640<<16, 360<<16
	}
	if err := marshalBox(mw.w, tkhd); err != nil {
		return err
	}
	if err := mw.end(); err != nil {
		return err
	}

	if err := mw.start(mp4.BoxTypeMdia()); err != nil {
		return err
	}
	if err := mw.start(mp4.BoxTypeMdhd()); err != nil {
		return err
	}
	mdhd := &mp4.Mdhd{Timescale: timescale, DurationV0: 0}
	if err := marshalBox(mw.w, mdhd); err != nil {
		return err
	}
	if err := mw.end(); err != nil {
		return err
	}
	if err := mw.start(mp4.BoxTypeHdlr()); err != nil {
		return err
	}
	if err := marshalBox(mw.w, &mp4.Hdlr{HandlerType: kindName, Name: handler}); err != nil {
		return err
	}
	if err := mw.end(); err != nil {
		return err
	}

	if err := mw.start(mp4.BoxTypeMinf()); err != nil {
		return err
	}
	if kind == trakVideo {
		if err := mw.start(mp4.BoxTypeVmhd()); err != nil {
			return err
		}
		if err := marshalBox(mw.w, &mp4.Vmhd{}); err != nil {
			return err
		}
	} else {
		if err := mw.start(mp4.BoxTypeSmhd()); err != nil {
			return err
		}
		if err := marshalBox(mw.w, &mp4.Smhd{}); err != nil {
			return err
		}
	}
	if err := mw.end(); err != nil {
		return err
	}
	if err := mw.dinf(); err != nil {
		return err
	}
	if err := mw.stbl(kind, samples); err != nil {
		return err
	}
	if err := mw.end(); err != nil { // minf
		return err
	}
	if err := mw.end(); err != nil { // mdia
		return err
	}
	return mw.end() // trak
}

// dinf 写出自包含的数据引用。
func (mw *mp4Writer) dinf() error {
	if err := mw.start(mp4.BoxTypeDinf()); err != nil {
		return err
	}
	if err := mw.start(mp4.BoxTypeDref()); err != nil {
		return err
	}
	if err := marshalBox(mw.w, &mp4.Dref{EntryCount: 1}); err != nil {
		return err
	}
	if err := mw.start(mp4.BoxTypeUrl()); err != nil {
		return err
	}
	if err := marshalBox(mw.w, &mp4.Url{}); err != nil {
		return err
	}
	if err := mw.end(); err != nil { // url
		return err
	}
	if err := mw.end(); err != nil { // dref
		return err
	}
	return mw.end() // dinf
}

// stbl 写出样本表：stsd/stts/stsc/stsz/stco/stss。stco 的偏移稍后回填。
func (mw *mp4Writer) stbl(kind trakKind, samples []samplePlan) error {
	if err := mw.start(mp4.BoxTypeStbl()); err != nil {
		return err
	}
	if err := mw.start(mp4.BoxTypeStsd()); err != nil {
		return err
	}
	if err := marshalBox(mw.w, &mp4.Stsd{EntryCount: 1}); err != nil {
		return err
	}
	if kind == trakVideo {
		if _, err := mw.w.Write(visualEntry(640, 360)); err != nil {
			return err
		}
	} else {
		if _, err := mw.w.Write(audioEntry(2, 48000)); err != nil {
			return err
		}
	}
	if err := mw.end(); err != nil {
		return err
	}

	if err := mw.start(mp4.BoxTypeStts()); err != nil {
		return err
	}
	entries := mergeDeltas(samples)
	if err := marshalBox(mw.w, &mp4.Stts{EntryCount: uint32(len(entries)), Entries: entries}); err != nil {
		return err
	}
	if err := mw.end(); err != nil {
		return err
	}

	if err := mw.start(mp4.BoxTypeStsc()); err != nil {
		return err
	}
	stsc := &mp4.Stsc{EntryCount: 1, Entries: []mp4.StscEntry{{FirstChunk: 1, SamplesPerChunk: uint32(len(samples)), SampleDescriptionIndex: 1}}}
	if err := marshalBox(mw.w, stsc); err != nil {
		return err
	}
	if err := mw.end(); err != nil {
		return err
	}

	if err := mw.start(mp4.BoxTypeStsz()); err != nil {
		return err
	}
	sizes := make([]uint32, len(samples))
	for i, s := range samples {
		sizes[i] = uint32(s.size)
	}
	if err := marshalBox(mw.w, &mp4.Stsz{SampleCount: uint32(len(samples)), EntrySize: sizes}); err != nil {
		return err
	}
	if err := mw.end(); err != nil {
		return err
	}

	// stco：写占位值，记录首个 ChunkOffset 字段的文件偏移供回填。
	if err := mw.start(mp4.BoxTypeStco()); err != nil {
		return err
	}
	pos, err := mw.f.Seek(0, 1)
	if err != nil {
		return err
	}
	mw.stcoPos = append(mw.stcoPos, pos+4+4) // FullBox(4) + EntryCount(4)
	if err := marshalBox(mw.w, &mp4.Stco{EntryCount: 1, ChunkOffset: []uint32{0}}); err != nil {
		return err
	}
	if err := mw.end(); err != nil {
		return err
	}

	// stss 仅在存在非全关键帧时写出。
	var keys []uint32
	allKey := true
	for i, s := range samples {
		if s.keyframe {
			keys = append(keys, uint32(i+1))
		} else {
			allKey = false
		}
	}
	if !allKey && len(keys) > 0 {
		if err := mw.start(mp4.BoxTypeStss()); err != nil {
			return err
		}
		if err := marshalBox(mw.w, &mp4.Stss{EntryCount: uint32(len(keys)), SampleNumber: keys}); err != nil {
			return err
		}
		if err := mw.end(); err != nil {
			return err
		}
	}
	return mw.end()
}

// mergeDeltas 把逐样本 delta 合并为 stts 游程。
func mergeDeltas(samples []samplePlan) []mp4.SttsEntry {
	var out []mp4.SttsEntry
	for _, s := range samples {
		if n := len(out); n > 0 && out[n-1].SampleDelta == s.delta {
			out[n-1].SampleCount++
			continue
		}
		out = append(out, mp4.SttsEntry{SampleCount: 1, SampleDelta: s.delta})
	}
	return out
}

// writeCorrupt 生成截断样例：保留 moov 与 mdat 头，截掉尾部数据。
func writeCorrupt(dst, src string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return fault.Wrap(fault.StageIO, 0, -1, false, "读取有效夹具失败", err)
	}
	if len(data) < 64 {
		return fault.New(fault.StageIO, 0, -1, false, "有效夹具过小，无法构造截断样例")
	}
	keep := len(data) * 2 / 3
	if keep >= len(data)-16 {
		keep = len(data) - 16
	}
	cut := append([]byte(nil), data[:keep]...)
	if err := os.WriteFile(dst, cut, 0o644); err != nil {
		return fault.Wrap(fault.StageIO, 0, -1, false, "写截断样例失败", err)
	}
	return nil
}
