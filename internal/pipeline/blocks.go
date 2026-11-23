// Index-block partition and merge helpers used by checkpointing.
package pipeline

import (
	"archive.local/archive/internal/checkpoint"
	"archive.local/archive/internal/repair"
)

func blockIDs(idx *repair.Index) []checkpoint.BlockID {
	var ids []checkpoint.BlockID
	for i := range idx.Tracks {
		ti := &idx.Tracks[i]
		for from := 0; from < len(ti.Entries); from += blockSize {
			to := from + blockSize
			if to > len(ti.Entries) {
				to = len(ti.Entries)
			}
			ids = append(ids, checkpoint.BlockID{TrackID: ti.TrackID, From: from, To: to})
		}
	}
	return ids
}

// completedBlocks merges previously completed blocks with freshly computed
// ones, so a resumed run keeps the progress it can still prove.
func completedBlocks(prior *checkpoint.State, idx *repair.Index) []checkpoint.Block {
	out := make([]checkpoint.Block, 0, len(blockIDs(idx)))
	seen := make(map[checkpoint.BlockID]bool)
	if prior != nil {
		for _, b := range prior.Completed {
			out = append(out, b)
			seen[b.ID] = true
		}
	}
	for _, id := range blockIDs(idx) {
		if seen[id] {
			continue
		}
		ti := findTrack(idx, id.TrackID)
		if ti == nil {
			continue
		}
		fp, err := ti.BlockFingerprint(id.From, id.To)
		if err != nil {
			continue
		}
		out = append(out, checkpoint.Block{ID: id, Fingerprint: fp})
	}
	return out
}

func findTrack(idx *repair.Index, id uint32) *repair.TrackIndex {
	for i := range idx.Tracks {
		if idx.Tracks[i].TrackID == id {
			return &idx.Tracks[i]
		}
	}
	return nil
}
