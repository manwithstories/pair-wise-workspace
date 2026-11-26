package parser

// TimeEntry is one stts time-to-sample run.
type TimeEntry struct {
	Count uint32
	Delta uint32
}

// STSCEntry is one sample-to-chunk run.
type STSCEntry struct {
	FirstChunk      uint32
	SamplesPerChunk uint32
	DescriptionID   uint32
}

func parseELST(src *Source, b *Box) []Edit {
	r := newTableReader(src, b)
	version, ok := r.u8()
	if !ok {
		return nil
	}
	_, _ = r.take(3)
	count, ok := r.u32()
	if !ok || count > 1_000_000 {
		return nil
	}
	out := make([]Edit, 0, count)
	for i := uint32(0); i < count; i++ {
		e := Edit{ByteOffset: b.Offset}
		if version == 1 {
			seg, ok1 := r.u64()
			mediaTime, ok2 := r.u64()
			rate, ok3 := r.u32()
			if !ok1 || !ok2 || !ok3 {
				return out
			}
			e.SegmentDuration, e.MediaTime, e.MediaRate = seg, int64(mediaTime), rate
		} else {
			seg, ok1 := r.u32()
			mediaTime, ok2 := r.u32()
			rate, ok3 := r.u32()
			if !ok1 || !ok2 || !ok3 {
				return out
			}
			e.SegmentDuration = uint64(seg)
			e.MediaTime = int64(int32(mediaTime))
			e.MediaRate = rate
		}
		out = append(out, e)
	}
	return out
}

func parseSTSZ(src *Source, b *Box, t *Track) {
	r := newTableReader(src, b)
	if _, ok := r.take(4); !ok { // version + flags
		return
	}
	uniform, ok1 := r.u32()
	count, ok2 := r.u32()
	if !ok1 || !ok2 {
		return
	}
	t.uniformSize = uniform
	t.sampleCount = count
	t.SampleCountKnown = true

	n := int(count)
	if maxByPayload := int(r.remaining() / 4); uniform == 0 && n > maxByPayload {
		n = maxByPayload // cursor validation still prevents any out-of-bounds read
	}
	t.Samples = make([]Sample, n)
	for i := range t.Samples {
		t.Samples[i].Number = i + 1
		if uniform != 0 {
			t.Samples[i].Size = uint64(uniform)
			continue
		}
		size, ok := r.u32()
		if !ok {
			t.Samples = t.Samples[:i]
			break
		}
		t.Samples[i].Size = uint64(size)
	}
}

func parseSTTS(src *Source, b *Box, t *Track) {
	r := newTableReader(src, b)
	if _, ok := r.take(4); !ok {
		return
	}
	count, ok := r.u32()
	if !ok || count > 10_000_000 {
		return
	}
	t.timeEntries = make([]TimeEntry, 0, count)
	for i := uint32(0); i < count; i++ {
		c, ok1 := r.u32()
		d, ok2 := r.u32()
		if !ok1 || !ok2 {
			break
		}
		t.timeEntries = append(t.timeEntries, TimeEntry{Count: c, Delta: d})
	}
}

func parseSTSS(src *Source, b *Box) []uint32 {
	r := newTableReader(src, b)
	if _, ok := r.take(4); !ok {
		return nil
	}
	count, ok := r.u32()
	if !ok || count > uint32(r.remaining()/4) {
		count = uint32(r.remaining() / 4)
	}
	out := make([]uint32, 0, count)
	for i := uint32(0); i < count; i++ {
		n, ok := r.u32()
		if !ok {
			break
		}
		out = append(out, n)
	}
	return out
}

func parseSTCO(src *Source, b *Box, wide bool) []OffsetEntry {
	r := newTableReader(src, b)
	if _, ok := r.take(4); !ok {
		return nil
	}
	count, ok := r.u32()
	if !ok {
		return nil
	}
	step := 4
	if wide {
		step = 8
	}
	if count > uint32(r.remaining()/int64(step)) {
		count = uint32(r.remaining() / int64(step))
	}
	out := make([]OffsetEntry, 0, count)
	for i := uint32(0); i < count; i++ {
		valueOffset := r.off
		if wide {
			v, ok := r.u64()
			if !ok {
				break
			}
			out = append(out, OffsetEntry{Value: v, ValueOffset: valueOffset, Wide: true})
		} else {
			v, ok := r.u32()
			if !ok {
				break
			}
			out = append(out, OffsetEntry{Value: uint64(v), ValueOffset: valueOffset})
		}
	}
	return out
}

func parseSTSC(src *Source, b *Box, t *Track) {
	r := newTableReader(src, b)
	if _, ok := r.take(4); !ok {
		return
	}
	count, ok := r.u32()
	if !ok || count > uint32(r.remaining()/12) {
		return
	}
	t.stscEntries = make([]STSCEntry, 0, count)
	for i := uint32(0); i < count; i++ {
		first, ok1 := r.u32()
		samples, ok2 := r.u32()
		desc, ok3 := r.u32()
		if !ok1 || !ok2 || !ok3 {
			break
		}
		t.stscEntries = append(t.stscEntries, STSCEntry{FirstChunk: first, SamplesPerChunk: samples, DescriptionID: desc})
	}
}

func finalizeSamples(t *Track) {
	decode := int64(0)
	idx := 0
	for _, run := range t.timeEntries {
		for j := uint32(0); j < run.Count && idx < len(t.Samples); j++ {
			t.Samples[idx].DecodeTime = decode
			t.Samples[idx].Delta = run.Delta
			decode += int64(run.Delta)
			idx++
		}
	}

	chunk := uint32(0)
	sampleInChunk := uint32(0)
	samplesPerChunk := uint32(1)
	chunkDataPos := uint64(0)
	for i := range t.Samples {
		if sampleInChunk == 0 {
			chunk++
			samplesPerChunk = samplesForChunk(t.stscEntries, chunk)
			chunkDataPos = 0
		}
		if int(chunk-1) < len(t.ChunkOffsets) {
			t.Samples[i].Offset = t.ChunkOffsets[chunk-1].Value + chunkDataPos
		}
		chunkDataPos += t.Samples[i].Size
		sampleInChunk++
		if sampleInChunk >= samplesPerChunk {
			sampleInChunk = 0
		}
	}
}

func samplesForChunk(entries []STSCEntry, chunk uint32) uint32 {
	spc := uint32(1)
	for _, e := range entries {
		if e.FirstChunk <= chunk {
			spc = e.SamplesPerChunk
		} else {
			break
		}
	}
	if spc == 0 {
		return 1
	}
	return spc
}

func markKeyframes(t *Track) {
	if len(t.KeyframeNumbers) == 0 {
		return
	}
	isKey := map[int]bool{}
	for _, n := range t.KeyframeNumbers {
		if n > 0 {
			isKey[int(n)] = true
		}
	}
	for i := range t.Samples {
		t.Samples[i].Keyframe = isKey[t.Samples[i].Number]
	}
}
