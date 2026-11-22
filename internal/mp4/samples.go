package mp4

import (
	"bytes"
	"fmt"

	gomp4 "github.com/abema/go-mp4"

	"mediad/internal/clock"
	"mediad/internal/fault"
	"mediad/internal/model"
)

// decode 用 go-mp4 反序列化叶子盒 payload。
func decode[T gomp4.IBox](n *BoxNode, dst T) (T, error) {
	if n == nil {
		return dst, fault.New(fault.KindRecoverable, fault.CodeIndexMissing, "box absent")
	}
	_, err := gomp4.Unmarshal(bytes.NewReader(n.Payload), uint64(len(n.Payload)), dst, gomp4.Context{})
	if err != nil {
		return dst, fault.Recoverable(fault.CodeCorrupt, "mp4.Unmarshal:"+n.Type, 0, 0, err)
	}
	return dst, nil
}

// BuildTracks 从盒树提取轨道与时基。
func BuildTracks(f *File) ([]model.Track, []*fault.Error) {
	var tracks []model.Track
	var faults []*fault.Error
	moov := f.Root.Find("moov")
	if moov == nil {
		return nil, append(faults, fault.Recoverable(fault.CodeTruncated,
			"mp4.BuildTracks", 0, 0, fmt.Errorf("moov box absent")))
	}
	for i, trak := range moov.FindAll("trak") {
		mdhdNode := trak.Find("mdhd")
		mdhd, err := decode(mdhdNode, &gomp4.Mdhd{})
		if err != nil {
			faults = append(faults, err.(*fault.Error))
			continue
		}
		handler := "unknown"
		if h := trak.Find("hdlr"); h != nil && len(h.Payload) >= 12 {
			handler = string(bytes.TrimRight(h.Payload[8:12], "\x00"))
		}
		tracks = append(tracks, model.Track{
			ID:        fmt.Sprintf("trk-%d", i+1),
			Handler:   handler,
			Timescale: mdhd.Timescale,
			Timebase:  clock.MustNew(int64(mdhd.Timescale), 1),
		})
	}
	return tracks, faults
}

// BuildSamples 展开单个 trak 的样本表，输出逐样本的 PTS、关键帧与完整性。
// trackID 由调用方（BuildTracks 的顺序）提供。
func BuildSamples(f *File, trak *BoxNode, trackID string) ([]model.Sample, []*fault.Error) {
	var faults []*fault.Error
	stbl := trak.Find("stbl")
	if stbl == nil {
		return nil, append(faults, fault.Recoverable(fault.CodeCorrupt,
			"mp4.BuildSamples", 0, 0, fmt.Errorf("stbl absent for %s", trackID)))
	}
	// stts：逐样本时长与 PTS。
	sttsNode := stbl.Find("stts")
	stts, err := decode(sttsNode, &gomp4.Stts{})
	if err != nil {
		return nil, append(faults, err.(*fault.Error))
	}
	// elst：负时间戳通过 MediaTime 偏移注入。
	ptsOffset := int64(0)
	if elst := trak.Find("elst"); elst != nil {
		if e, derr := decode(elst, &gomp4.Elst{}); derr == nil && e.EntryCount > 0 {
			ptsOffset = e.GetMediaTime(0)
		}
	}
	type dur struct {
		n      uint32
		delta  uint32
	}
	durs := make([]dur, 0, stts.EntryCount)
	var total uint32
	for _, en := range stts.Entries {
		durs = append(durs, dur{en.SampleCount, en.SampleDelta})
		total += en.SampleCount
	}
	// stss：关键帧样本号（1 基）；缺失时索引层负责重建。
	keySet := map[uint32]bool{}
	if stss := stbl.Find("stss"); stss != nil {
		if s, derr := decode(stss, &gomp4.Stss{}); derr == nil {
			for _, sn := range s.SampleNumber {
				keySet[sn] = true
			}
		} else {
			faults = append(faults, derr.(*fault.Error))
		}
	}
	// stsz：逐样本大小。
	stsz, err := decode(stbl.Find("stsz"), &gomp4.Stsz{})
	if err != nil {
		return nil, append(faults, err.(*fault.Error))
	}
	sizes := stsz.EntrySize
	if stsz.SampleSize != 0 {
		sizes = make([]uint32, stsz.SampleCount)
		for i := range sizes {
			sizes[i] = stsz.SampleSize
		}
	}
	if uint32(len(sizes)) != total {
		faults = append(faults, fault.Recoverable(fault.CodeCorrupt,
			"mp4.stsz", 0, 0, fmt.Errorf(
				"sample count mismatch: stts=%d stsz=%d", total, len(sizes))))
	}
	// stsc + stco/co64：chunk -> 文件偏移，再展开逐样本偏移。
	offsets, cerr := chunkOffsets(stbl)
	if cerr != nil {
		faults = append(faults, cerr)
	}
	stsc, err := decode(stbl.Find("stsc"), &gomp4.Stsc{})
	if err != nil {
		return nil, append(faults, err.(*fault.Error))
	}
	sampleOff := expandOffsets(offsets, stsc.Entries, sizes)
	samples := make([]model.Sample, 0, total)
	pts := ptsOffset
	var idx uint32 = 1
	for _, d := range durs {
		for j := uint32(0); j < d.n; j++ {
			s := model.Sample{
				TrackID: trackID, Index: idx, PTS: pts,
				Duration: d.delta, Keyframe: keySet[idx], Complete: true,
			}
			if int(idx-1) < len(sizes) {
				s.Size = sizes[idx-1]
			}
			if int(idx-1) < len(sampleOff) {
				s.Offset = sampleOff[idx-1]
			}
			samples = append(samples, s)
			pts += int64(d.delta)
			idx++
		}
	}
	// 全部样本展开后再统一做 mdat 边界校验：截断只标记首个受影响样本
	// 及其后续区间，避免把局部故障扩散成整条轨道的故障洪泛。
	// 边界取 mdat 声明结尾与文件实际 EOF 的较小值，尾部截断因此可见。
	mdatDeclEnd, mdatStart := mdatBounds(f)
	mdatEnd := mdatDeclEnd
	if f.RawSize > 0 && uint64(f.RawSize) < mdatEnd {
		mdatEnd = uint64(f.RawSize)
	}
	for i := range samples {
		s := &samples[i]
		if mdatEnd == 0 || s.Size == 0 || int(s.Index-1) >= len(sampleOff) {
			continue
		}
		if s.Offset < mdatStart || uint64(s.Offset)+uint64(s.Size) > mdatEnd {
			s.Complete = false
			faults = append(faults, fault.Recoverable(fault.CodeTruncated,
				"mp4.mdatBounds", s.PTS, s.PTS+int64(s.Duration),
				fmt.Errorf("sample %d outside mdat", s.Index)))
		}
	}
	return samples, faults
}

func chunkOffsets(stbl *BoxNode) ([]uint64, *fault.Error) {
	if co := stbl.Find("co64"); co != nil {
		c, err := decode(co, &gomp4.Co64{})
		if err != nil {
			return nil, err.(*fault.Error)
		}
		return c.ChunkOffset, nil
	}
	c, err := decode(stbl.Find("stco"), &gomp4.Stco{})
	if err != nil {
		return nil, err.(*fault.Error)
	}
	out := make([]uint64, len(c.ChunkOffset))
	for i, v := range c.ChunkOffset {
		out[i] = uint64(v)
	}
	return out, nil
}

// expandOffsets 按 stsc 规则把 chunk 偏移展开为逐样本字节偏移。
func expandOffsets(chunks []uint64, entries []gomp4.StscEntry, sizes []uint32) []uint64 {
	out := make([]uint64, 0, len(sizes))
	si := 0 // 已消费的样本数（全局，跨 chunk 连续编号）
	for chunk := uint32(0); int(chunk) < len(chunks); chunk++ {
		perChunk := samplesPerChunk(entries, chunk)
		off := chunks[chunk]
		for k := uint32(0); k < perChunk; k++ {
			out = append(out, off)
			if si < len(sizes) {
				off += uint64(sizes[si])
			}
			si++
		}
	}
	return out
}

func samplesPerChunk(entries []gomp4.StscEntry, chunk uint32) uint32 {
	per := uint32(1)
	for _, e := range entries {
		if e.FirstChunk <= chunk+1 { // stsc 的 first_chunk 为 1 基
			per = e.SamplesPerChunk
		}
	}
	return per
}

func mdatBounds(f *File) (end, start uint64) {
	m := f.Root.Find("mdat")
	if m == nil {
		return 0, 0
	}
	return m.Offset + m.Size, m.Offset + m.Header
}
