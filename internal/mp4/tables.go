package mp4

import (
	"mediasync/internal/model"
)

// readStsz expands stsz into per-sample sizes.
func (r *Reader) readStsz(kids []hdr, trackID uint32) ([]uint32, error) {
	h, ok := find(kids, "stsz")
	if !ok {
		return nil, model.NewError(model.ErrTruncatedBox, trackID, 0, "stbl has no stsz")
	}
	buf, err := r.payload(h)
	if err != nil {
		return nil, r.classify(err)
	}
	if len(buf) < 12 {
		return nil, model.NewError(model.ErrTruncatedBox, trackID, int64(h.offset), "stsz payload too short")
	}
	fixed := be32(buf[4:8])
	count := be32(buf[8:12])
	if count > 1<<24 {
		return nil, model.NewError(model.ErrSizeLimit, trackID, int64(h.offset), "stsz sample count %d too large", count)
	}
	out := make([]uint32, count)
	if fixed != 0 {
		for i := range out {
			out[i] = fixed
		}
		return out, nil
	}
	need := 12 + int(count)*4
	if len(buf) < need {
		return nil, model.NewError(model.ErrTruncatedBox, trackID, int64(h.offset), "stsz declares %d entries, table truncated", count)
	}
	for i := uint32(0); i < count; i++ {
		out[i] = be32(buf[12+int(i)*4:])
	}
	return out, nil
}

// readStts expands the time-to-sample table into per-sample durations.
func (r *Reader) readStts(kids []hdr, trackID uint32) ([]uint32, error) {
	h, ok := find(kids, "stts")
	if !ok {
		return nil, model.NewError(model.ErrTruncatedBox, trackID, 0, "stbl has no stts")
	}
	buf, err := r.payload(h)
	if err != nil {
		return nil, r.classify(err)
	}
	if len(buf) < 8 {
		return nil, model.NewError(model.ErrTruncatedBox, trackID, int64(h.offset), "stts payload too short")
	}
	entries := be32(buf[4:8])
	if int(entries)*8 > len(buf)-8 {
		return nil, model.NewError(model.ErrTruncatedBox, trackID, int64(h.offset), "stts declares %d entries, table truncated", entries)
	}
	var out []uint32
	for i := uint32(0); i < entries; i++ {
		off := 8 + int(i)*8
		n := be32(buf[off:])
		delta := be32(buf[off+4:])
		if n > 1<<24 || uint64(len(out))+uint64(n) > 1<<24 {
			return nil, model.NewError(model.ErrSizeLimit, trackID, int64(h.offset), "stts expands beyond limit")
		}
		for j := uint32(0); j < n; j++ {
			out = append(out, delta)
		}
	}
	return out, nil
}

// readCtts expands the composition offset table. Absent box means zero offsets.
func (r *Reader) readCtts(kids []hdr, trackID uint32) ([]int32, error) {
	h, ok := find(kids, "ctts")
	if !ok {
		return nil, nil
	}
	buf, err := r.payload(h)
	if err != nil {
		return nil, r.classify(err)
	}
	if len(buf) < 8 {
		return nil, model.NewError(model.ErrTruncatedBox, trackID, int64(h.offset), "ctts payload too short")
	}
	version := buf[0]
	entries := be32(buf[4:8])
	if int(entries)*8 > len(buf)-8 {
		return nil, model.NewError(model.ErrTruncatedBox, trackID, int64(h.offset), "ctts declares %d entries, table truncated", entries)
	}
	var out []int32
	for i := uint32(0); i < entries; i++ {
		off := 8 + int(i)*8
		n := be32(buf[off:])
		raw := be32(buf[off+4:])
		var v int32
		if version == 0 {
			v = int32(raw)
		} else {
			v = int32(raw)
		}
		for j := uint32(0); j < n; j++ {
			out = append(out, v)
		}
	}
	return out, nil
}

// readChunks reads stco/co64 plus stsc into chunk offsets and samples-per-chunk.
func (r *Reader) readChunks(kids []hdr, trackID uint32) ([]uint64, []uint32, error) {
	var offsets []uint64
	if h, ok := find(kids, "stco"); ok {
		buf, err := r.payload(h)
		if err != nil {
			return nil, nil, r.classify(err)
		}
		if len(buf) < 8 {
			return nil, nil, model.NewError(model.ErrTruncatedBox, trackID, int64(h.offset), "stco payload too short")
		}
		n := be32(buf[4:8])
		if int(n)*4 > len(buf)-8 {
			return nil, nil, model.NewError(model.ErrTruncatedBox, trackID, int64(h.offset), "stco declares %d offsets, table truncated", n)
		}
		for i := uint32(0); i < n; i++ {
			offsets = append(offsets, uint64(be32(buf[8+int(i)*4:])))
		}
	} else if h, ok := find(kids, "co64"); ok {
		buf, err := r.payload(h)
		if err != nil {
			return nil, nil, r.classify(err)
		}
		if len(buf) < 8 {
			return nil, nil, model.NewError(model.ErrTruncatedBox, trackID, int64(h.offset), "co64 payload too short")
		}
		n := be32(buf[4:8])
		if int(n)*8 > len(buf)-8 {
			return nil, nil, model.NewError(model.ErrTruncatedBox, trackID, int64(h.offset), "co64 declares %d offsets, table truncated", n)
		}
		for i := uint32(0); i < n; i++ {
			offsets = append(offsets, be64(buf[8+int(i)*8:]))
		}
	} else {
		return nil, nil, model.NewError(model.ErrTruncatedBox, trackID, 0, "stbl has no stco/co64")
	}

	h, ok := find(kids, "stsc")
	if !ok {
		return nil, nil, model.NewError(model.ErrTruncatedBox, trackID, 0, "stbl has no stsc")
	}
	buf, err := r.payload(h)
	if err != nil {
		return nil, nil, r.classify(err)
	}
	if len(buf) < 8 {
		return nil, nil, model.NewError(model.ErrTruncatedBox, trackID, int64(h.offset), "stsc payload too short")
	}
	entries := be32(buf[4:8])
	if int(entries)*12 > len(buf)-8 {
		return nil, nil, model.NewError(model.ErrTruncatedBox, trackID, int64(h.offset), "stsc declares %d entries, table truncated", entries)
	}
	type sc struct{ first, per uint32 }
	var scs []sc
	for i := uint32(0); i < entries; i++ {
		off := 8 + int(i)*12
		scs = append(scs, sc{first: be32(buf[off:]), per: be32(buf[off+4:])})
	}
	perChunk := make([]uint32, len(offsets))
	for i := range perChunk {
		perChunk[i] = ^uint32(0) // unassigned
	}
	for i, e := range scs {
		if e.first == 0 {
			return nil, nil, model.NewError(model.ErrTruncatedBox, trackID, int64(h.offset), "stsc first_chunk is 1-based, got 0")
		}
		start := int(e.first) - 1
		end := len(offsets)
		if i+1 < len(scs) {
			end = int(scs[i+1].first) - 1
			if end > len(offsets) {
				end = len(offsets)
			}
		}
		if start > len(offsets) {
			start = len(offsets)
		}
		for c := start; c < end; c++ {
			perChunk[c] = e.per
		}
	}
	for c, v := range perChunk {
		if v == ^uint32(0) {
			return nil, nil, model.NewError(model.ErrTruncatedBox, trackID, int64(h.offset), "chunk %d has no stsc entry", c)
		}
	}
	return offsets, perChunk, nil
}

// buildSamples lays out sample offsets by walking chunks and per-chunk counts.
func buildSamples(trackID uint32, sizes, durations []uint32, ctts []int32, chunks []uint64, perChunk []uint32, fileSize uint64, mdat hdr) ([]model.Sample, error) {
	n := len(sizes)
	if len(durations) != n {
		return nil, model.NewError(model.ErrTruncatedBox, trackID, 0,
			"stts describes %d samples but stsz declares %d", len(durations), n)
	}
	if ctts != nil && len(ctts) != n {
		return nil, model.NewError(model.ErrTruncatedBox, trackID, 0,
			"ctts describes %d samples but stsz declares %d", len(ctts), n)
	}
	samples := make([]model.Sample, n)
	var dts int64
	idx := 0
	for ci, off := range chunks {
		cur := off
		for k := uint32(0); k < perChunk[ci]; k++ {
			if idx >= n {
				return nil, model.NewError(model.ErrTruncatedBox, trackID, int64(off),
					"chunk table describes more samples than stsz (%d)", n)
			}
			size := sizes[idx]
			if uint64(size) > maxSampleBytes {
				return nil, model.NewError(model.ErrSizeLimit, trackID, int64(cur), "sample %d size %d exceeds limit", idx, size)
			}
			end := cur + uint64(size)
			if end > fileSize {
				return nil, model.NewError(model.ErrTruncatedBox, trackID, int64(cur),
					"sample %d ends at %d past input size %d", idx, end, fileSize)
			}
			if end > mdat.offset+mdat.total {
				return nil, model.NewError(model.ErrTruncatedBox, trackID, int64(cur),
					"sample %d extends past mdat", idx)
			}
			s := model.Sample{Index: idx, Offset: int64(cur), Size: int(size), DTS: dts, Duration: int64(durations[idx])}
			s.PTS = dts
			if ctts != nil {
				s.PTS = dts + int64(ctts[idx])
			}
			samples[idx] = s
			dts += int64(durations[idx])
			cur = end
			idx++
		}
	}
	if idx != n {
		return nil, model.NewError(model.ErrTruncatedBox, trackID, 0,
			"chunk table describes %d samples but stsz declares %d", idx, n)
	}
	return samples, nil
}
