// Track reading and clock repair, plus the manifest/source identity helpers.
package pipeline

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"archive.local/archive/internal/manifest"
	"archive.local/archive/internal/model"
	mp4 "archive.local/archive/internal/mp4"
	"archive.local/archive/internal/repair"
)

func readAndRepair(mf *manifest.Manifest, tl *model.Timeline, res *Result) (map[uint32][]repair.Sample, []*model.Error, error) {
	tracks := make(map[uint32][]repair.Sample, len(mf.Entries))
	var defects []*model.Error
	maxGap := int64(tl.Timescale) * 60 // a minute of missing media
	for _, e := range mf.Entries {
		path, err := mf.ResolveSource(e)
		if err != nil {
			return nil, nil, asModelError(err)
		}
		tr, perr := mp4.ParseFile(path, e.TrackID, tl)
		if perr != nil {
			me := asModelError(perr)
			// Attach the manifest line and declared track so a source defect
			// is traceable back to the entry that named it.
			if me.Line == 0 {
				me.Line = e.Line
			}
			if me.Track == 0 {
				me.Track = e.TrackID
			}
			defects = append(defects, me)
			if tr == nil || len(tr.Samples) == 0 {
				continue
			}
		}
		if tr == nil {
			continue
		}
		usable := tr.Samples
		if tr.Complete < len(tr.Samples) {
			usable = tr.Samples[:tr.Complete]
		}
		rep, rrep, cerr := repair.RepairClock(&mp4.Track{TrackID: tr.TrackID, Timescale: tr.Timescale,
			Origin: tr.Origin, Samples: usable, Sync: tr.Sync}, tl, maxGap)
		if cerr != nil {
			defects = append(defects, asModelError(cerr))
		}
		tracks[e.TrackID] = rep
		res.Tracks = append(res.Tracks, trackStats(e.TrackID, rep, rrep))
	}
	if len(tracks) == 0 {
		// No track could be read. Prefer the concrete first defect (a missing
		// or unreadable source) over a generic internal error so the caller
		// reports the real cause and location.
		if len(defects) > 0 {
			return nil, defects, defects[0]
		}
		return nil, defects, &model.Error{Code: model.CodeInternal, Sample: -1, OffsetBytes: -1,
			Detail: "no track could be read"}
	}
	return tracks, defects, nil
}

func sourceIdentity(mf *manifest.Manifest, manifestDigest string) string {
	h := sha256.New()
	h.Write([]byte(manifestDigest))
	for _, e := range mf.Entries {
		fmt.Fprintf(h, "|%d:%s:%d:%d", e.TrackID, e.Source, e.Timescale, e.OriginTicks)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func trackStats(id uint32, samples []repair.Sample, rep repair.ClockReport) TrackStats {
	s := TrackStats{TrackID: id, Samples: len(samples), Regressed: rep.Regressed,
		Missing: rep.Missing, Gaps: rep.Gaps}
	for _, sm := range samples {
		if sm.Sync {
			s.Keyframes++
		}
	}
	if len(samples) > 0 {
		s.FirstUnified = samples[0].Unified
		s.LastUnified = samples[len(samples)-1].Unified
	}
	return s
}
