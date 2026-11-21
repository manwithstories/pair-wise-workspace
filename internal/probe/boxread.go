package probe

import (
	"bytes"

	"github.com/abema/go-mp4"
)

// maxHeadBytes bounds the moov region kept in memory; single-file read
// cache stays well under the 64 MiB project constraint.
const maxHeadBytes = 32 << 20

// readTrackBox dispatches one stbl/mdhd/edts payload into idx.
func readTrackBox(h *mp4.ReadHandle, idx *rawIndex) error {
	box, _, err := h.ReadPayload()
	if err != nil {
		return err
	}
	switch b := box.(type) {
	case *mp4.Tkhd:
		idx.trackID = b.TrackID
	case *mp4.Mdhd:
		idx.timescale = b.Timescale
		if b.GetVersion() == 1 {
			idx.duration = int64(b.DurationV1)
		} else {
			idx.duration = int64(b.DurationV0)
		}
	case *mp4.Hdlr:
		idx.isVideo = string(b.HandlerType[:]) == "vide"
	case *mp4.Elst:
		idx.edits = decodeEdits(b)
	case *mp4.Stts:
		idx.stts = append(idx.stts, b.Entries...)
	case *mp4.Stss:
		idx.haveSTSS = true
		for _, n := range b.SampleNumber {
			idx.keyframes[n] = struct{}{}
		}
	case *mp4.Stsz:
		idx.sizes = decodeSizes(b)
	case *mp4.Stco:
		idx.chunks = make([]int64, len(b.ChunkOffset))
		for i, o := range b.ChunkOffset {
			idx.chunks[i] = int64(o)
		}
	case *mp4.Co64:
		for _, o := range b.ChunkOffset {
			idx.chunks = append(idx.chunks, int64(o))
		}
	case *mp4.Stsc:
		for _, e := range b.Entries {
			idx.samplesPerChunk = append(idx.samplesPerChunk, e.SamplesPerChunk)
		}
	}
	return nil
}

func decodeEdits(elst *mp4.Elst) []Edit {
	out := make([]Edit, 0, elst.EntryCount)
	for _, e := range elst.Entries {
		if elst.GetVersion() == 1 {
			mt := e.MediaTimeV1
			out = append(out, Edit{
				SegmentDuration: int64(e.SegmentDurationV1),
				MediaTime:       mt,
				MediaRate:       int64(e.MediaRateInteger),
			})
			continue
		}
		out = append(out, Edit{
			SegmentDuration: int64(e.SegmentDurationV0),
			MediaTime:       int64(e.MediaTimeV0),
			MediaRate:       int64(e.MediaRateInteger),
		})
	}
	return out
}

func decodeSizes(stsz *mp4.Stsz) []uint32 {
	if stsz.SampleSize != 0 {
		out := make([]uint32, stsz.SampleCount)
		for i := range out {
			out[i] = stsz.SampleSize
		}
		return out
	}
	return append([]uint32(nil), stsz.EntrySize...)
}

// collectIndexes walks the captured head buffer and returns one rawIndex
// per trak. It is split from Parse so the walk closure stays small and the
// 300-line file limit is respected.
func collectIndexes(head []byte) []*rawIndex {
	seeker := bytes.NewReader(head)
	var indexes []*rawIndex
	var cur *rawIndex
	_, _ = mp4.ReadBoxStructure(seeker, func(h *mp4.ReadHandle) (interface{}, error) {
		switch h.BoxInfo.Type.String() {
		case "moov", "mdia", "minf", "stbl", "edts":
			// Structural containers: descend without changing cur.
			return h.Expand()
		case "trak":
			cur = &rawIndex{keyframes: map[uint32]struct{}{}}
			indexes = append(indexes, cur)
			return h.Expand()
		case "tkhd", "mdhd", "hdlr", "elst", "stts", "stss", "stsz", "stco", "co64", "stsc":
			if cur != nil {
				if err := readTrackBox(h, cur); err != nil {
					return nil, err
				}
			}
		}
		return nil, nil
	})
	return indexes
}
