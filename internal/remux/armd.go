package remux

import (
	"encoding/binary"
	"sort"

	"mediaarchive/internal/clock"
	"mediaarchive/internal/demux"
	"mediaarchive/internal/model"
)

// armd binary layout mirrors internal/demux/mov.go. Keep the two encoders in
// lockstep: the demuxer is this writer's reference parser.
type sampleLoc struct {
	chunkSeq  uint32
	order     int // order inside its chunk table
	offsetPos int // byte position of the offset field inside armd payload
}

// buildArmd serializes the archive index. baseOffset is the absolute mdat
// payload position; call 0 first to measure length, then finalizeOffsets
// patches the real values without changing the serialized size.
func buildArmd(tracks []demux.TrackMeta, samples []clock.NormalizedSample,
	damages []Damage, baseOffset int64) ([]byte, []sampleLoc, error) {

	chunks := groupChunks(samples)
	var b []byte
	put := func(p []byte) { b = append(b, p...) }
	put([]byte("ARMD"))
	b = append(b, 1) // version
	b = append(b, 0) // flags
	b = binary.BigEndian.AppendUint16(b, uint16(len(tracks)))
	for _, t := range tracks {
		b = binary.BigEndian.AppendUint32(b, t.ID)
		b = append(b, demux.KindByte(t.Kind))
		b = binary.BigEndian.AppendUint32(b, t.Timescale)
		b = binary.BigEndian.AppendUint64(b, t.StartTime)
		b = binary.BigEndian.AppendUint32(b, uint32(t.DriftPPM))
		b = binary.BigEndian.AppendUint32(b, t.WrapUnits)
		b = binary.BigEndian.AppendUint32(b, t.RTPCount)
		if len(t.CodecHint) > 255 {
			return nil, nil, model.NewError(model.StageRemux, "codec hint too long")
		}
		b = append(b, byte(len(t.CodecHint)))
		put([]byte(t.CodecHint))
	}
	b = binary.BigEndian.AppendUint32(b, uint32(len(chunks)))
	var locs []sampleLoc
	running := baseOffset
	for _, ch := range chunks {
		b = binary.BigEndian.AppendUint32(b, ch.seq)
		b = binary.BigEndian.AppendUint32(b, uint32(len(ch.samples)))
		for i, s := range ch.samples {
			b = binary.BigEndian.AppendUint32(b, s.TrackID)
			b = binary.BigEndian.AppendUint32(b, s.Seq)
			b = binary.BigEndian.AppendUint64(b, s.DTS)
			b = binary.BigEndian.AppendUint32(b, s.Duration)
			locs = append(locs, sampleLoc{
				chunkSeq: ch.seq, order: i, offsetPos: len(b),
			})
			b = binary.BigEndian.AppendUint64(b, uint64(running))
			running += int64(len(s.Payload))
			b = binary.BigEndian.AppendUint32(b, s.Size)
			b = binary.BigEndian.AppendUint32(b, s.Flags)
			b = binary.BigEndian.AppendUint32(b, s.RTPSeq)
		}
	}
	// Damage log (extension after the sample tables; the demux parser reads
	// lazily and tolerates the trailing section).
	b = binary.BigEndian.AppendUint32(b, uint32(len(damages)))
	for _, d := range damages {
		b = binary.BigEndian.AppendUint32(b, uint32(d.TrackID))
		b = binary.BigEndian.AppendUint64(b, uint64(d.Offset))
		b = binary.BigEndian.AppendUint32(b, d.SampleSeq)
		reason := []byte(d.Reason)
		if len(reason) > 255 {
			reason = reason[:255]
		}
		b = append(b, byte(len(reason)))
		put(reason)
	}
	return b, locs, nil
}

type chunkGroup struct {
	seq     uint32
	samples []clock.NormalizedSample
}

func groupChunks(samples []clock.NormalizedSample) []chunkGroup {
	idx := map[uint32]int{}
	var out []chunkGroup
	for _, s := range samples {
		i, ok := idx[s.ChunkSeq]
		if !ok {
			idx[s.ChunkSeq] = len(out)
			out = append(out, chunkGroup{seq: s.ChunkSeq})
			i = len(out) - 1
		}
		out[i].samples = append(out[i].samples, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].seq < out[j].seq })
	return out
}

// finalizeOffsets rewrites each sample's absolute offset. Payload order in
// mdat follows the merged sample order, chunk by chunk.
func finalizeOffsets(armd []byte, locs []sampleLoc, mdatPayloadPos int64) ([]byte, error) {
	running := mdatPayloadPos
	// Re-derive sizes by reading them back from the placeholder table; this
	// stays independent of buildArmd's running counter.
	for _, loc := range locs {
		sizePos := loc.offsetPos + 8
		if sizePos+4 > len(armd) {
			return nil, model.NewError(model.StageRemux, "armd offset patch out of bounds")
		}
		size := binary.BigEndian.Uint32(armd[sizePos:])
		binary.BigEndian.PutUint64(armd[loc.offsetPos:], uint64(running))
		running += int64(size)
	}
	return armd, nil
}
