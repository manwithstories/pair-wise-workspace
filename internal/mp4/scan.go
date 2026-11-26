// Package mp4 scans MP4/MOV containers in a streaming fashion: box
// payloads of media data (mdat) are skipped by seeking, never loaded
// into memory. Only container timing and sample tables are parsed.
package mp4

import (
	"errors"
	"io"
	"os"

	gomp4 "github.com/abema/go-mp4"

	"mediaroute/internal/model"
)

// RawTrack is the parsed container structure of a single-track file.
type RawTrack struct {
	Timescale    uint32
	Duration     uint64 // mdhd duration, media timescale units
	Stts         []gomp4.SttsEntry
	Ctts         []gomp4.CttsEntry
	CttsVersion  uint8
	SampleSizes  []uint32 // expanded per-sample sizes
	Stsc         []gomp4.StscEntry
	ChunkOffsets []uint64
	Sync         []uint32 // stss entries (1-based sample numbers)
	HasStss      bool
	Elst         []gomp4.ElstEntry
	HasElst      bool
	ElstVersion  uint8
	MdatOffset   uint64 // absolute offset of mdat payload
	MdatBytes    uint64 // payload bytes actually present on disk
	FileSize     uint64
	Faults       []model.Fault
	Gaps         []model.Gap
}

// ScanFile parses the container structure of path. Media payloads are
// never read. Truncated or overflowing boxes are recorded as recoverable
// faults with exact offsets; a missing moov/mdhd is unrecoverable.
func ScanFile(path string) (*RawTrack, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, model.New(model.CodeIO, "scan open "+path, err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, model.New(model.CodeIO, "scan stat "+path, err)
	}
	rt := &RawTrack{FileSize: uint64(st.Size())}
	for {
		pos, _ := f.Seek(0, io.SeekCurrent)
		bi, err := gomp4.ReadBoxInfo(f)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil { // partial box header at tail: garbage, ignore rest
			rt.addFault(model.FaultBoxOverflow, uint64(pos), "truncated box header")
			break
		}
		if bi.Size < bi.HeaderSize {
			rt.addFault(model.FaultBoxOverflow, bi.Offset, "declared size smaller than header")
			break
		}
		if end := bi.Offset + bi.Size; !bi.ExtendToEOF && end > rt.FileSize {
			rt.scanOverflow(bi)
			break // everything past a corrupt box is unusable
		}
		switch bi.Type {
		case gomp4.BoxTypeMoov():
			if err := rt.scanMoov(f, bi); err != nil {
				return nil, err
			}
		case gomp4.BoxTypeMdat():
			rt.MdatOffset = bi.Offset + bi.HeaderSize
			rt.MdatBytes = bi.Size - bi.HeaderSize
			if bi.ExtendToEOF {
				rt.MdatBytes = rt.FileSize - rt.MdatOffset
			}
		}
		if _, err := bi.SeekToEnd(f); err != nil {
			return nil, model.New(model.CodeIO, "scan seek "+path, err)
		}
	}
	if rt.Timescale == 0 {
		return nil, model.Newf(model.CodeUnrecoverable, "scan "+path,
			"no moov/mdhd found")
	}
	return rt, nil
}

// scanOverflow handles a box whose declared end lies beyond EOF.
func (rt *RawTrack) scanOverflow(bi *gomp4.BoxInfo) {
	if bi.Type == gomp4.BoxTypeMdat() {
		rt.MdatOffset = bi.Offset + bi.HeaderSize
		rt.MdatBytes = 0
		if rt.FileSize > rt.MdatOffset {
			rt.MdatBytes = rt.FileSize - rt.MdatOffset
		}
		missing := bi.Size - bi.HeaderSize - rt.MdatBytes
		rt.addFault(model.FaultTruncatedMdat, rt.FileSize,
			"mdat declares more payload than the file holds")
		rt.Gaps = append(rt.Gaps, model.Gap{
			Offset: rt.FileSize, Bytes: missing, Reason: string(model.FaultTruncatedMdat)})
		return
	}
	rt.addFault(model.FaultBoxOverflow, bi.Offset,
		"box "+bi.Type.String()+" declares size beyond end of file")
}

func (rt *RawTrack) addFault(code model.FaultCode, offset uint64, detail string) {
	rt.Faults = append(rt.Faults, model.Fault{Code: code, Offset: offset, Detail: detail})
}

// scanMoov walks the moov subtree and unmarshals only the boxes needed
// to rebuild timing and the random-access index.
func (rt *RawTrack) scanMoov(r io.ReadSeeker, bi *gomp4.BoxInfo) error {
	_, err := gomp4.ReadBoxStructureFromInternal(r, bi,
		func(h *gomp4.ReadHandle) (interface{}, error) {
			switch h.BoxInfo.Type {
			case gomp4.BoxTypeMoov(), gomp4.BoxTypeTrak(), gomp4.BoxTypeMdia(),
				gomp4.BoxTypeMinf(), gomp4.BoxTypeStbl(), gomp4.BoxTypeEdts():
				_, err := h.Expand()
				return nil, err
			}
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			switch b := box.(type) {
			case *gomp4.Mdhd:
				if rt.Timescale == 0 { // first track wins
					rt.Timescale = b.Timescale
					rt.Duration = b.GetDuration()
				}
			case *gomp4.Stts:
				rt.Stts = b.Entries
			case *gomp4.Ctts:
				rt.Ctts = b.Entries
				rt.CttsVersion = b.Version
			case *gomp4.Stsz:
				rt.SampleSizes = expandStsz(b)
			case *gomp4.Stsc:
				rt.Stsc = b.Entries
			case *gomp4.Stco:
				rt.ChunkOffsets = make([]uint64, len(b.ChunkOffset))
				for i, o := range b.ChunkOffset {
					rt.ChunkOffsets[i] = uint64(o)
				}
			case *gomp4.Co64:
				rt.ChunkOffsets = b.ChunkOffset
			case *gomp4.Stss:
				rt.HasStss = true
				rt.Sync = b.SampleNumber
			case *gomp4.Elst:
				rt.HasElst = true
				rt.Elst = b.Entries
				rt.ElstVersion = b.Version
			}
			return nil, nil
		})
	if err != nil {
		return model.New(model.CodeUnrecoverable, "scan moov", err)
	}
	return nil
}

func expandStsz(b *gomp4.Stsz) []uint32 {
	n := int(b.SampleCount)
	sizes := make([]uint32, n)
	if b.SampleSize != 0 { // constant size
		for i := range sizes {
			sizes[i] = b.SampleSize
		}
		return sizes
	}
	copy(sizes, b.EntrySize)
	return sizes
}

// Expand builds the per-sample table: file offsets from stsc/stco/stsz,
// raw DTS from stts, PTS from ctts, sync flags from stss, and truncation
// flags from the actual mdat extent. DTS here is raw (possibly
// non-monotonic); monotonic repair happens in the repair package.
func (rt *RawTrack) Expand() []model.Sample {
	n := len(rt.SampleSizes)
	samples := make([]model.Sample, n)
	// DTS / PTS
	var dts int64
	idx := 0
	for _, e := range rt.Stts {
		for k := uint32(0); k < e.SampleCount && idx < n; k++ {
			samples[idx].Index = uint32(idx)
			samples[idx].DTS = dts
			samples[idx].PTS = dts
			dts += int64(e.SampleDelta)
			idx++
		}
	}
	idx = 0
	for _, e := range rt.Ctts {
		for k := uint32(0); k < e.SampleCount && idx < n; k++ {
			if rt.CttsVersion == 0 {
				samples[idx].PTS += int64(e.SampleOffsetV0)
			} else {
				samples[idx].PTS += int64(e.SampleOffsetV1)
			}
			idx++
		}
	}
	// sizes and offsets
	rt.expandOffsets(samples)
	// sync flags
	rt.expandSync(samples)
	// truncation flags
	end := rt.MdatOffset + rt.MdatBytes
	for i := range samples {
		samples[i].Size = rt.SampleSizes[i]
		if samples[i].Offset < rt.MdatOffset ||
			samples[i].Offset+uint64(samples[i].Size) > end {
			samples[i].Truncated = true
		}
	}
	return samples
}

func (rt *RawTrack) expandOffsets(samples []model.Sample) {
	n := len(samples)
	sampleIdx := 0
	for ci, coff := range rt.ChunkOffsets {
		perChunk := uint32(0)
		for _, e := range rt.Stsc {
			if uint32(ci+1) >= e.FirstChunk {
				perChunk = e.SamplesPerChunk
			}
		}
		off := coff
		for k := uint32(0); k < perChunk && sampleIdx < n; k++ {
			samples[sampleIdx].Offset = off
			off += uint64(rt.SampleSizes[sampleIdx])
			sampleIdx++
		}
	}
}

func (rt *RawTrack) expandSync(samples []model.Sample) {
	n := uint32(len(samples))
	if !rt.HasStss { // ISO fallback: no stss means every sample is sync
		for i := range samples {
			samples[i].Keyframe = true
		}
		return
	}
	valid := 0
	for _, sn := range rt.Sync {
		if sn >= 1 && sn <= n {
			samples[sn-1].Keyframe = true
			valid++
		}
	}
	if valid == 0 && n > 0 {
		// stss present but no usable entry: recover via the same
		// ISO fallback so random access stays possible.
		rt.addFault(model.FaultNoSync, 0, "stss has no usable sync entry")
		for i := range samples {
			samples[i].Keyframe = true
		}
	}
}
