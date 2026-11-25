package container

import (
	"github.com/abema/go-mp4"

	"relay.local/relay/internal/fault"
)

// layoutChunks resolves sample offsets from the chunk tables.
func (p *mp4Parser) layoutChunks(st *trakState) {
	trak := st.track
	if st.hadStts && st.hadStsz && len(st.chunkOffsets) == 0 && len(trak.Samples) > 0 {
		p.faults.Add(fault.New(trak.Number, 0, fault.KindCorrupt, false,
			"track has %d timed samples but no chunk offset table", len(trak.Samples)))
		return
	}
	if len(st.chunkOffsets) == 0 || len(st.stsc) == 0 || len(trak.Samples) == 0 {
		return
	}
	// Validate stsc monotonicity once.
	for i := 1; i < len(st.stsc); i++ {
		if st.stsc[i].FirstChunk <= st.stsc[i-1].FirstChunk {
			p.faults.Add(fault.New(trak.Number, 0, fault.KindCorrupt, false,
				"stsc first-chunk values are not increasing"))
			return
		}
	}
	sampleIdx := 0
	for chunk := 1; chunk <= len(st.chunkOffsets) && sampleIdx < len(trak.Samples); chunk++ {
		spc := samplesInChunk(st.stsc, uint32(chunk))
		if spc == 0 {
			p.faults.Add(fault.New(trak.Number, 0, fault.KindCorrupt, true,
				"stsc maps chunk %d to zero samples", chunk))
			return
		}
		off := int64(st.chunkOffsets[chunk-1])
		for i := uint32(0); i < spc && sampleIdx < len(trak.Samples); i++ {
			s := &trak.Samples[sampleIdx]
			s.Offset = off
			off += s.Size
			sampleIdx++
		}
	}
	if sampleIdx < len(trak.Samples) {
		p.faults.Add(fault.New(trak.Number, 0, fault.KindCorrupt, true,
			"chunk tables cover %d of %d samples", sampleIdx, len(trak.Samples)))
	}
}

// samplesInChunk returns how many samples a chunk holds per the stsc table.
func samplesInChunk(entries []mp4.StscEntry, chunk uint32) uint32 {
	var spc uint32
	for _, e := range entries {
		if e.FirstChunk > chunk {
			break
		}
		spc = e.SamplesPerChunk
	}
	return spc
}

// finalize validates sample ranges and fills derived track fields.
func (p *mp4Parser) finalize(st *trakState) {
	trak := st.track
	p.layoutChunks(st)
	if st.hadStts && len(st.syncSet) > 0 {
		// A present stss replaces the implicit "every sample is sync" default.
		for i := range trak.Samples {
			trak.Samples[i].Sync = st.syncSet[i]
		}
	}
	trak.Kind = st.kind
	for i := range trak.Samples {
		s := &trak.Samples[i]
		s.TrackID = trak.ID
		s.Codec = trak.Codec
		if s.Size == 0 {
			continue
		}
		if s.Offset < 0 || s.End() > int64(len(p.data)) {
			p.faults.Add(fault.New(trak.Number, s.Offset, fault.KindOutOfRange, true,
				"sample %d range [%d,%d) outside file of %d bytes", s.Index, s.Offset, s.End(), len(p.data)))
			s.Size = 0
			s.Offset = 0
		}
	}
	if trak.Duration == 0 && len(trak.Samples) > 0 {
		last := trak.Samples[len(trak.Samples)-1]
		trak.Duration = last.DTS + last.Duration
	}
}
