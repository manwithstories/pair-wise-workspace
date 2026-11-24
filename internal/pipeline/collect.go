package pipeline

import (
	"mediadeliver/internal/index"
	"mediadeliver/internal/manifest"
	"mediadeliver/internal/model"
)

// cutStats accumulates keyframe cut-point request outcomes. A request is a
// segment's video edit start; it is a hit only when a real keyframe exists at
// or before that timestamp. Non-keyframes are never counted as hits.
type cutStats struct {
	requests int
	hits     int
}

// collectSegmentPackets selects, for every mapped track, the packets whose
// DTS fall inside the segment's edit windows (source timescale), preserving
// per-track order and the non-zero timeline origin. It also accounts cut
// points through the keyframe index.
func (rn *Runner) collectSegmentPackets(man *manifest.Manifest, segName string,
	mappings []*model.TrackMapping,
	demuxed map[string]map[uint32][]*model.Packet,
	kfIndex *index.KeyframeIndex) ([]*model.Packet, error) {

	stats := rn.cutStatsFor(segName)
	var out []*model.Packet

	for _, mp := range mappings {
		trackPkts := demuxed[mp.MaterialID][mp.SourceTrack]
		edits := editsFor(man, segName, mp.MaterialID, mp.SourceTrack)
		if len(edits) == 0 {
			// No explicit edit: take the whole mapped track.
			out = append(out, trackPkts...)
			if mp.Kind == model.KindVideo {
				rn.accountCut(stats, kfIndex, mp, firstDTS(trackPkts))
			}
			continue
		}
		for _, ed := range edits {
			start, err := ed.StartTicks()
			if err != nil {
				return nil, err
			}
			end, err := ed.EndTicks()
			if err != nil {
				return nil, err
			}
			if mp.Kind == model.KindVideo {
				rn.accountCut(stats, kfIndex, mp, start)
			}
			for _, p := range trackPkts {
				// Half-open [start,end); origin stays in source ticks.
				if p.DTS >= start && p.DTS < end {
					out = append(out, p)
				}
			}
		}
	}
	return out, nil
}

// accountCut records one video cut-point request and whether a real keyframe
// can serve it.
func (rn *Runner) accountCut(stats *cutStats, kfIndex *index.KeyframeIndex,
	mp *model.TrackMapping, targetDTS int64) {
	stats.requests++
	if _, ok := kfIndex.Lookup(mp.MaterialID, mp.SourceTrack, targetDTS); ok {
		stats.hits++
	}
}

// cutStatsFor returns the per-segment stats holder, creating it on first use.
func (rn *Runner) cutStatsFor(seg string) *cutStats {
	if rn.cut == nil {
		rn.cut = map[string]*cutStats{}
	}
	st := rn.cut[seg]
	if st == nil {
		st = &cutStats{}
		rn.cut[seg] = st
	}
	return st
}

// KeyframeHitRate returns the aggregate cut-point hit percentage. When no
// video cut was requested, every (audio-only or empty) run is reported as a
// perfect 100% because there were no missable requests.
func (rn *Runner) KeyframeHitRate() float64 {
	req, hits := 0, 0
	for _, st := range rn.cut {
		req += st.requests
		hits += st.hits
	}
	if req == 0 {
		return 100
	}
	return float64(hits) / float64(req) * 100.0
}

// editsFor returns the edit entries bound to one segment/material/track.
func editsFor(man *manifest.Manifest, seg, materialID string, trackID uint32) []*manifest.EditEntry {
	var out []*manifest.EditEntry
	for _, e := range man.Edits {
		if e.Segment == seg && e.MaterialID == materialID && e.TrackID == trackID {
			out = append(out, e)
		}
	}
	return out
}

func firstDTS(pkts []*model.Packet) int64 {
	if len(pkts) == 0 {
		return 0
	}
	return pkts[0].DTS
}
