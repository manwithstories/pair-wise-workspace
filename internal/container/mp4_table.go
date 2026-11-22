// Sample-table parsing and expansion: stts, ctts, stsz, stco/co64, stsc.
//
// Expansion maps every sample index to an absolute file offset and DTS
// using integer arithmetic. The first sample that would be read outside the
// file is reported at the exact byte offset it would have occupied.
package container

import (
	"github.com/abema/go-mp4"

	"mediacore/internal/media"
)

func (st *parseState) readStts(tb *trackBuild, bi boxHeader) error {
	var box mp4.Stts
	if err := st.decodeBox(bi, &box); err != nil {
		return err
	}
	tb.stts = box.Entries
	return nil
}

func (st *parseState) readCtts(tb *trackBuild, bi boxHeader) error {
	var box mp4.Ctts
	if err := st.decodeBox(bi, &box); err != nil {
		return err
	}
	tb.ctts = box.Entries
	tb.cttsVersion = box.GetVersion()
	return nil
}

func (st *parseState) readStsz(tb *trackBuild, bi boxHeader) error {
	var box mp4.Stsz
	if err := st.decodeBox(bi, &box); err != nil {
		return err
	}
	if int(box.SampleCount) > st.opts.MaxSamplesPerTrack {
		return media.NewError(media.CodeMemoryLimit, bi.start, 0,
			"stsz sample count %d above limit", box.SampleCount)
	}
	tb.sampleSize = box.SampleSize
	tb.sampleSizes = box.EntrySize
	return nil
}

func (st *parseState) readStco(tb *trackBuild, bi boxHeader, co64 bool) error {
	body, err := readPayload(st.src.r, bi)
	if err != nil {
		return err
	}
	const n = 8
	entry := 4
	if co64 {
		entry = 8
	}
	if len(body) < n {
		return media.NewError(media.CodeInvalidBoxLength, bi.start, 0, "chunk offset table too short")
	}
	count := int(be32(body[4:8]))
	if count > st.opts.MaxChunkCount {
		return media.NewError(media.CodeMemoryLimit, bi.start, 0, "chunk count %d above limit", count)
	}
	if len(body) < n+count*entry {
		return media.NewError(media.CodeInvalidBoxLength, bi.start, 0,
			"chunk offset table declares %d entries, only %d bytes", count, len(body)-n)
	}
	offs := make([]uint64, count)
	for i := 0; i < count; i++ {
		if co64 {
			offs[i] = be64(body[n+i*8 : n+i*8+8])
		} else {
			offs[i] = uint64(be32(body[n+i*4 : n+i*4+4]))
		}
	}
	tb.chunkOffsets = offs
	return nil
}

func (st *parseState) readStsc(tb *trackBuild, bi boxHeader) error {
	var box mp4.Stsc
	if err := st.decodeBox(bi, &box); err != nil {
		return err
	}
	for _, e := range box.Entries {
		if int(e.FirstChunk) > st.opts.MaxChunkCount {
			return media.NewError(media.CodeMemoryLimit, bi.start, 0,
				"stsc first chunk %d above limit", e.FirstChunk)
		}
	}
	tb.stsc = box.Entries
	return nil
}

// finishTrack expands a completed audio track and appends it to the source.
func (st *parseState) finishTrack(tb *trackBuild, trak boxHeader) error {
	if tb.timescale <= 0 {
		return media.NewError(media.CodeUnsupportedTrack, trak.start, 0,
			"audio track %d has no mdhd timescale", tb.id)
	}
	sampleCount := len(tb.sampleSizes)
	if sampleCount == 0 {
		for _, e := range tb.stts {
			sampleCount += int(e.SampleCount)
		}
	}
	if len(tb.stts) == 0 {
		return media.NewError(media.CodeMissingBox, trak.start, 0, "audio track missing stts")
	}
	if sampleCount > st.opts.MaxSamplesPerTrack {
		return media.NewError(media.CodeMemoryLimit, trak.start, 0,
			"track sample count %d above limit", sampleCount)
	}
	if sampleCount > 0 && (len(tb.chunkOffsets) == 0 || len(tb.stsc) == 0) {
		return media.NewError(media.CodeMissingBox, trak.start, 0, "audio track missing chunk tables")
	}
	number := len(st.src.tracks) + 1
	refs, firstOff, err := buildSampleRefs(tb, sampleCount, st.fileSize)
	if err != nil {
		if me, ok := media.AsError(err); ok {
			me.Track = number
			return me
		}
		return err
	}
	tr := Track{
		Number:            number,
		ID:                tb.id,
		Timescale:         tb.timescale,
		Duration:          tb.duration,
		Handler:           tb.handler,
		SampleCount:       sampleCount,
		CodecTag:          tb.codecTag,
		SampleRate:        tb.rate,
		Channels:          tb.channels,
		BitsPerSample:     tb.bits,
		Config:            tb.config,
		FirstSampleOffset: firstOff,
	}
	st.src.tracks = append(st.src.tracks, tr)
	st.src.refs = append(st.src.refs, refs)
	return nil
}
