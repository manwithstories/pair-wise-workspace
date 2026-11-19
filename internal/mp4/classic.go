package mp4

import (
	"fmt"

	"github.com/abema/go-mp4"

	"archive/internal/errs"
)

// sampleTable collects the classic (non-fragmented) sample table boxes of
// one track. The demuxer fills it while walking moov/trak/mdia/minf/stbl
// and materializes samples from it when the file has no moof boxes.
type sampleTable struct {
	stts *mp4.Stts
	stsc *mp4.Stsc
	stsz *mp4.Stsz
	stco *mp4.Stco
	co64 *mp4.Co64
	ctts *mp4.Ctts
	stss *mp4.Stss
}

func (t *sampleTable) ready() bool {
	return t != nil && t.stts != nil && t.stsc != nil && t.stsz != nil &&
		(t.stco != nil || t.co64 != nil)
}

// chunkOffsets returns the chunk offsets from stco or co64.
func (t *sampleTable) chunkOffsets() []uint64 {
	if t.co64 != nil {
		return t.co64.ChunkOffset
	}
	out := make([]uint64, len(t.stco.ChunkOffset))
	for i, v := range t.stco.ChunkOffset {
		out[i] = uint64(v)
	}
	return out
}

// samplesPerChunk returns the stsc run length for 1-based chunk index.
func (t *sampleTable) samplesPerChunk(chunk uint32) uint32 {
	n := uint32(0)
	for _, e := range t.stsc.Entries {
		if e.FirstChunk > chunk {
			break
		}
		n = e.SamplesPerChunk
	}
	return n
}

// buildClassicSamples materializes samples for every track with a
// complete sample table. Payload offsets are bounds-checked, so a
// truncated file is reported with the offending byte offset.
func (d *demuxer) buildClassicSamples() error {
	for _, t := range d.tracks {
		tbl := d.tables[t.ID]
		if !tbl.ready() {
			continue
		}
		if t.Timescale == 0 {
			return errs.Newf(errs.KindDemux, d.asset, fmt.Sprintf("%s track %d", d.name, t.ID),
				"sample table present but mdhd timescale is zero")
		}
		t.ClockBits = 64
		chunks := tbl.chunkOffsets()
		// duration per sample, in stts run-length form
		var durs []uint32
		for _, e := range tbl.stts.Entries {
			for i := uint32(0); i < e.SampleCount; i++ {
				durs = append(durs, e.SampleDelta)
			}
		}
		// composition offsets per sample
		var ctos []int32
		if tbl.ctts != nil {
			for _, e := range tbl.ctts.Entries {
				for i := uint32(0); i < e.SampleCount; i++ {
					if tbl.ctts.GetVersion() == 0 {
						ctos = append(ctos, int32(e.SampleOffsetV0))
					} else {
						ctos = append(ctos, e.SampleOffsetV1)
					}
				}
			}
		}
		sync := map[uint32]bool{}
		if tbl.stss != nil {
			for _, n := range tbl.stss.SampleNumber {
				sync[n] = true
			}
		}
		total := int(tbl.stsz.SampleCount)
		if uint32(len(durs)) != tbl.stsz.SampleCount {
			return errs.Newf(errs.KindDemux, d.asset, fmt.Sprintf("%s track %d", d.name, t.ID),
				"stts declares %d samples, stsz declares %d", len(durs), total)
		}
		dts := int64(0)
		n := uint32(0) // 1-based sample number
		for ci, off := range chunks {
			chunk := uint32(ci + 1)
			per := tbl.samplesPerChunk(chunk)
			for k := uint32(0); k < per && n < tbl.stsz.SampleCount; k++ {
				size := tbl.stsz.SampleSize
				if size == 0 {
					size = tbl.stsz.EntrySize[n]
				}
				end := off + uint64(size)
				if size == 0 || end > uint64(len(d.data)) {
					return errs.Newf(errs.KindDemux, d.asset,
						fmt.Sprintf("%s box offset %d", d.name, off),
						"sample data [%d,%d) exceeds segment size %d (truncated?)",
						off, end, len(d.data))
				}
				var cto int32
				if int(n) < len(ctos) {
					cto = ctos[n]
				}
				key := len(sync) == 0 || sync[n+1]
				t.Samples = append(t.Samples, Sample{
					DTS: dts, CTSOffset: cto, Duration: durs[n],
					Keyframe: key, Data: d.data[off:end],
				})
				dts += int64(durs[n])
				off = end
				n++
			}
		}
		if n != tbl.stsz.SampleCount {
			return errs.Newf(errs.KindDemux, d.asset, fmt.Sprintf("%s track %d", d.name, t.ID),
				"chunk map covers %d of %d samples", n, tbl.stsz.SampleCount)
		}
	}
	return nil
}
