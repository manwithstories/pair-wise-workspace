package mp4

import "encoding/binary"

// buildTrak 构造单条 trak 盒的全部子盒（tkhd/edts/mdia/minf/stbl）。
func buildTrak(b *trackBuild, chunkOff uint32, fault Fault) []byte {
	sp := b.spec
	duration := sp.Count * sp.Delta

	tkhd := buildTkhd(1, duration)
	mdhd := buildMdhd(sp.Timescale, duration)
	hdlr := buildHdlr(sp.Handler)

	var stbl []byte
	stbl = append(stbl, buildStsd()...)
	stts := fullBox("stts", 0, 0,
		append(append(u32(1), u32(sp.Count)...), u32(sp.Delta)...))
	stbl = append(stbl, stts...)
	// stss：索引缺页故障下直接不写。
	if fault != FaultIndexMissing && len(b.keys) > 0 {
		payload := u32(uint32(len(b.keys)))
		for _, k := range b.keys {
			payload = append(payload, u32(k)...)
		}
		stbl = append(stbl, fullBox("stss", 0, 0, payload)...)
	}
	stbl = append(stbl, buildStsz(b.sizes, fault)...)
	// 单 chunk 容纳全部样本：stsc 首条目 first_chunk=1，
	// samples_per_chunk=样本总数，与 stco 的单一条目对应。
	perChunk := uint32(len(b.sizes))
	if perChunk == 0 {
		perChunk = 1
	}
	stsc := fullBox("stsc", 0, 0,
		append(append(append(u32(1), u32(1)...), u32(perChunk)...), u32(1)...))
	stbl = append(stbl, stsc...)
	stco := fullBox("stco", 0, 0, append(u32(1), u32(chunkOff)...))
	stbl = append(stbl, stco...)

	mediaHeader := buildSmhd()
	if sp.Handler == "vide" {
		mediaHeader = buildVmhd()
	}
	dinf := box("dinf", box("dref", buildDref()))
	minf := box("minf", append(append(mediaHeader, dinf...), box("stbl", stbl)...))
	mdia := box("mdia", append(append(mdhd, hdlr...), minf...))

	trak := append(tkhd, mdia...)
	if sp.MediaStart != 0 || fault == FaultNegativePTS {
		mt := sp.MediaStart
		if fault == FaultNegativePTS && mt == 0 {
			mt = -int64(sp.Delta)
		}
		trak = append(trak, buildEdts(mt, duration)...)
	}
	return trak
}

func buildTkhd(trackID, duration uint32) []byte {
	var p []byte
	p = append(p, u32(0)...)          // creation
	p = append(p, u32(0)...)          // modification
	p = append(p, u32(trackID)...)    // track_ID
	p = append(p, u32(0)...)          // reserved
	p = append(p, u32(duration)...)   // track duration
	p = append(p, make([]byte, 8)...) // reserved
	p = append(p, u16(0)...)          // layer
	p = append(p, u16(0)...)          // alternate group
	p = append(p, u16(0)...)          // volume
	p = append(p, u16(0)...)          // reserved
	matrix := []uint32{0x10000, 0, 0, 0, 0x10000, 0, 0, 0, 0x40000000}
	for _, m := range matrix {
		p = append(p, u32(m)...)
	}
	p = append(p, u32(320<<16)...) // width
	p = append(p, u32(240<<16)...) // height
	return fullBox("tkhd", 0, 3, p)
}

func buildMdhd(timescale, duration uint32) []byte {
	var p []byte
	p = append(p, u32(0)...)      // creation
	p = append(p, u32(0)...)      // modification
	p = append(p, u32(timescale)...)
	p = append(p, u32(duration)...)
	p = append(p, u16(0x55C4)...) // language 'und'
	p = append(p, u16(0)...)      // pre_defined
	return fullBox("mdhd", 0, 0, p)
}

func buildHdlr(handler string) []byte {
	var p []byte
	p = append(p, u32(0)...)
	p = append(p, fourccBytes(handler)...)
	p = append(p, make([]byte, 12)...)
	p = append(p, []byte(handler+"-handler\x00")...)
	return fullBox("hdlr", 0, 0, p)
}

func buildStsd() []byte {
	// 极简 stsd：entry_count=0，go-mp4 与本包解析器都接受空样本描述。
	return fullBox("stsd", 0, 0, u32(0))
}

func buildStsz(sizes []uint32, fault Fault) []byte {
	count := uint32(len(sizes))
	if fault == FaultCorrupt {
		// 声明比实际多两个样本，与 stts 计数冲突：payload 只放真实条目，
		// go-mp4 读到 EOF 即报损坏，而计数本身保持不一致。
		count += 2
	}
	p := append(u32(0), u32(count)...)
	for _, s := range sizes {
		p = append(p, u32(s)...)
	}
	return fullBox("stsz", 0, 0, p)
}

func buildEdts(mediaTime int64, duration uint32) []byte {
	var p []byte
	p = append(p, u32(1)...)                // entry_count
	p = append(p, u32(duration)...)         // segment_duration
	p = append(p, i32(int32(mediaTime))...) // media_time（可负）
	p = append(p, u16(1)...)                // rate integer
	p = append(p, u16(0)...)                // rate fraction
	return box("edts", fullBox("elst", 0, 0, p))
}

func buildSmhd() []byte { return fullBox("smhd", 0, 0, make([]byte, 4)) }
func buildVmhd() []byte { return fullBox("vmhd", 0, 1, make([]byte, 8)) }

func buildDref() []byte {
	entry := box("url ", fullBox("url ", 0, 1, nil))
	return append(u32(1), entry...)
}

func u16(v uint16) []byte {
	b := make([]byte, 2)
	binary.BigEndian.PutUint16(b, v)
	return b
}
