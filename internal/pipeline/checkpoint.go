// Checkpoint seeding for the interrupted-transfer demo path.
package pipeline

import (
	"path/filepath"

	"archive.local/archive/internal/cache"
	"archive.local/archive/internal/checkpoint"
	"archive.local/archive/internal/manifest"
	mp4 "archive.local/archive/internal/mp4"
	"archive.local/archive/internal/repair"
)

// CheckpointFor builds the checkpoint an interrupted transfer would leave:
// the manifest is parsed and indexed exactly as the resumed run will, then a
// partial block set is recorded. A later resume must re-validate all of it.
func CheckpointFor(work, manifestPath string, confirmedPackets int) (checkpoint.State, error) {
	if !filepath.IsAbs(manifestPath) {
		manifestPath = filepath.Join(work, manifestPath)
	}
	mf, err := manifest.ParseFileWithRoot(manifestPath, work)
	if err != nil {
		return checkpoint.State{}, err
	}
	digest, err := cache.FileDigest(manifestPath)
	if err != nil {
		return checkpoint.State{}, err
	}
	tl, err := mf.Timeline()
	if err != nil {
		return checkpoint.State{}, err
	}
	tracks := make(map[uint32][]repair.Sample, len(mf.Entries))
	for _, e := range mf.Entries {
		p, err := mf.ResolveSource(e)
		if err != nil {
			return checkpoint.State{}, err
		}
		tr, _ := mp4.ParseFile(p, e.TrackID, tl)
		if tr == nil {
			continue
		}
		usable := tr.Samples
		if tr.Complete < len(tr.Samples) {
			usable = tr.Samples[:tr.Complete]
		}
		rep, _, cerr := repair.RepairClock(&mp4.Track{TrackID: tr.TrackID, Timescale: tr.Timescale,
			Origin: tr.Origin, Samples: usable, Sync: tr.Sync}, tl, int64(tl.Timescale)*60)
		if cerr != nil && len(rep) == 0 {
			continue
		}
		tracks[e.TrackID] = rep
	}
	idx, err := repair.BuildIndex(tracks, mf.TrackIDs())
	if err != nil {
		return checkpoint.State{}, err
	}
	st := checkpoint.State{
		InputDigest:   digest,
		SourceID:      sourceIdentity(mf, digest),
		ConfirmedHigh: int64(confirmedPackets),
		IndexDigest:   idx.Digest,
	}
	// Record only the first block: the interruption happened mid-run.
	ids := blockIDs(idx)
	if len(ids) > 0 {
		ti := findTrack(idx, ids[0].TrackID)
		if ti != nil {
			if fp, err := ti.BlockFingerprint(ids[0].From, ids[0].To); err == nil {
				st.MarkCompleted(ids[0], fp)
			}
		}
	}
	return st, nil
}
