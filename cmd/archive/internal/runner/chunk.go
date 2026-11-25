package runner

import (
	"encoding/hex"
	"sort"

	"mediaarchive/internal/checkpoint"
	"mediaarchive/internal/clock"
)

// commitChunks advances the checkpoint only at whole-chunk boundaries. A
// chunk is committed only when the validated set matches the full sample
// count the container declares for it; a partial chunk (e.g. after
// --stop-after) leaves no trace, so a resume reprocesses it from scratch and
// stays idempotent.
func commitChunks(state *checkpoint.State, expected map[uint32]int,
	collected []clock.NormalizedSample, lastOrder uint64) error {

	groups := map[uint32][]checkpoint.SampleRecord{}
	for _, s := range collected {
		rec := checkpoint.SampleRecord{
			TrackID: s.TrackID, Seq: s.Seq, ChunkSeq: s.ChunkSeq,
			DTS: s.DTS, Duration: s.Duration, Offset: s.Offset, Size: s.Size,
			Flags: s.Flags, RTPSeq: s.RTPSeq,
			DTSNormNS: s.DTSNorm.Nanoseconds(),
			SHA256:    hex.EncodeToString(s.SHA256[:]),
		}
		groups[s.ChunkSeq] = append(groups[s.ChunkSeq], rec)
	}
	seqs := make([]uint32, 0, len(groups))
	for seq := range groups {
		seqs = append(seqs, seq)
	}
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
	var total uint64
	for _, seq := range seqs {
		recs := groups[seq]
		if want, ok := expected[seq]; ok && len(recs) < want {
			continue // partial chunk: do not commit any boundary
		}
		sort.Slice(recs, func(i, j int) bool {
			if recs[i].TrackID != recs[j].TrackID {
				return recs[i].TrackID < recs[j].TrackID
			}
			return recs[i].Seq < recs[j].Seq
		})
		state.MarkChunk(seq, recs, lastOrder, 0)
		total += uint64(len(recs))
	}
	state.SamplesCommitted = total
	return nil
}
