package pipeline

import (
	"os"
	"path/filepath"

	"avpipe/internal/checkpoint"
	"avpipe/internal/fixture"
	"avpipe/internal/hls"
	"avpipe/internal/media"
)

// RunResume replays valid checkpoints, removes orphans and rebuilds missing
// chunks. Deterministic payload hashes establish idempotence.
func RunResume(cli Options) (*Report, error) {
	opt, err := LoadOptions(cli.Out)
	if err != nil {
		return nil, err
	}
	opt.Out = cli.Out
	rep, _, err := BuildPlan(opt)
	if err != nil {
		return rep, err
	}
	store, err := checkpoint.Open(opt.Out)
	if err != nil {
		return rep, err
	}
	idx, verified, bad, removed, err := store.Reconcile()
	if err != nil {
		return rep, err
	}
	if len(bad) > 0 {
		return rep, media.NewError(media.CodeCheckpoint, -1, "committed hash mismatch: %v", bad)
	}
	if idx.ConfigHash != rep.ConfigHash || idx.SourceHash != rep.SourceHash {
		return rep, media.NewError(media.CodeCheckpoint, -1,
			"checkpoint provenance does not match regenerated source/config")
	}
	reused := len(idx.Chunks)
	rebuilt := 0
	for _, slot := range rep.Ladder.Slots {
		if idx.Chunks[slot.Seq] != nil {
			continue
		}
		rec, err := buildChunk(opt, rep, store, slot.Seq)
		if err != nil {
			return rep, err
		}
		if err := store.CommitChunk(rec); err != nil {
			return rep, err
		}
		idx.Chunks[rec.Seq] = rec
		rebuilt++
	}
	verified, bad, err = store.Verify(idx)
	if err != nil {
		return rep, err
	}
	if len(bad) > 0 {
		return rep, media.NewError(media.CodeCheckpoint, -1, "rebuilt hash mismatch: %v", bad)
	}
	rep.CheckpointReused = reused
	rep.Rebuilt = rebuilt
	rep.OrphansRemoved = removed
	rep.SegmentHashOK = verified
	rep.SegmentHashTotal = countSegmentRecords(idx)
	rep.Idempotent = idempotenceCheck(rep, idx)
	if _, err := WriteManifests(opt.Out, rep, idx); err != nil {
		return rep, err
	}
	rep.Chunks = idx.Chunks
	_ = WriteJSON(filepath.Join(opt.Out, ReportFile), rep)
	_ = WriteJSON(filepath.Join(opt.Out, ProvenanceFile), rep)
	return rep, nil
}

func idempotenceCheck(rep *Report, idx *checkpoint.Index) int {
	for _, slot := range rep.Ladder.Slots {
		for _, b := range slot.Boundaries {
			rec := findSegment(idx, slot.Seq, b.Rendition)
			cleanBoundary := b.Present && len(b.Reasons) == 0
			if !cleanBoundary {
				if rec != nil {
					return 0
				}
				continue
			}
			if rec == nil {
				return 0
			}
			a := AnalysisByName(rep, b.Rendition)
			if a == nil {
				return 0
			}
			data, _, _ := SegmentPayload(a, slot.Seq)
			if fixture.HashData(data) != rec.SHA256 {
				return 0
			}
		}
	}
	return 1
}

// RunVerify reloads provenance, checks segment hashes and parses manifests.
func RunVerify(opt Options) (*Report, error) {
	saved, err := LoadOptions(opt.Out)
	if err != nil {
		return nil, err
	}
	saved.Out = opt.Out
	rep, _, err := BuildPlan(saved)
	if err != nil {
		return rep, err
	}
	store, err := checkpoint.Open(opt.Out)
	if err != nil {
		return rep, err
	}
	idx, verified, bad, _, err := store.Reconcile()
	if err != nil {
		return rep, err
	}
	if len(bad) > 0 {
		return rep, media.NewError(media.CodeCheckpoint, -1, "hash mismatch: %v", bad)
	}
	rep.SegmentHashOK = verified
	rep.SegmentHashTotal = countSegmentRecords(idx)
	in := BuildManifestInput(rep, idx)
	master, err := os.ReadFile(filepath.Join(opt.Out, "master.m3u8"))
	if err != nil {
		return rep, media.WrapError(media.CodeCheckpoint, -1, err, "read master playlist")
	}
	rt, err := hls.VerifyRoundTrip(master, in)
	if err != nil {
		return rep, err
	}
	for _, rv := range in.Renditions {
		data, err := os.ReadFile(filepath.Join(opt.Out, rv.PlaylistFile))
		if err != nil {
			return rep, err
		}
		checked, matched, err := hls.VerifyMedia(data, rv)
		if err != nil {
			return rep, err
		}
		rt.Checked += checked
		rt.Matched += matched
	}
	rep.RoundTripChecked = rt.Checked
	rep.RoundTripMatched = rt.Matched
	rep.Idempotent = idempotenceCheck(rep, idx)
	rep.Chunks = idx.Chunks
	_ = WriteJSON(filepath.Join(opt.Out, ReportFile), rep)
	return rep, nil
}
