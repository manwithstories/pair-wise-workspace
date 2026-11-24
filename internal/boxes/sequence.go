package boxes

import "sort"

// AnalyzeSequences detects missing, out-of-order, duplicate mfhd and exact
// content duplicates.  Content duplicates are marked via DuplicateOf in the
// returned unique list order.
func AnalyzeSequences(frags []*Fragment) SequenceReport {
	rep := SequenceReport{}
	byTrack := map[uint32][]*Fragment{}
	for _, f := range frags {
		if f == nil || f.Isolated {
			continue
		}
		byTrack[f.TrackID] = append(byTrack[f.TrackID], f)
	}
	trackIDs := make([]uint32, 0, len(byTrack))
	for id := range byTrack {
		trackIDs = append(trackIDs, id)
	}
	sort.Slice(trackIDs, func(i, j int) bool { return trackIDs[i] < trackIDs[j] })

	for _, trackID := range trackIDs {
		fs := byTrack[trackID]
		seenSeq := map[uint32]*Fragment{}
		var seqValues []uint32
		var prevSeq uint32
		for idx, f := range fs {
			loc := Loc{TrackID: trackID, Sequence: f.Sequence, Fragment: f.No, Offset: f.MoofOffset, Path: f.Path}
			if first, ok := seenSeq[f.Sequence]; ok {
				rep.DuplicateMFHD = append(rep.DuplicateMFHD, loc)
				_ = first
			} else {
				seenSeq[f.Sequence] = f
				seqValues = append(seqValues, f.Sequence)
			}
			if idx > 0 && f.Sequence < prevSeq {
				rep.OutOfOrder = append(rep.OutOfOrder, loc)
			}
			prevSeq = f.Sequence
		}
		if len(seqValues) > 0 {
			min, max := seqValues[0], seqValues[0]
			present := map[uint32]bool{}
			for _, s := range seqValues {
				present[s] = true
				if s < min {
					min = s
				}
				if s > max {
					max = s
				}
			}
			missing := MissingSequences{TrackID: trackID}
			// Guard against pathological uint32 ranges.
			if max-min < 1_000_000 {
				for s := min; s <= max; s++ {
					if !present[s] {
						missing.Sequences = append(missing.Sequences, s)
					}
					if s == max {
						break
					}
				}
			}
			if len(missing.Sequences) > 0 {
				rep.Missing = append(rep.Missing, missing)
			}
		}
	}

	seenHash := map[string]*Fragment{}
	for _, f := range frags {
		if f == nil || f.Isolated || f.ContentSHA256 == "" {
			continue
		}
		loc := Loc{TrackID: f.TrackID, Sequence: f.Sequence, Fragment: f.No, Offset: f.MoofOffset, Path: f.Path}
		if first, ok := seenHash[f.ContentSHA256]; ok {
			rep.ContentDuplicate = append(rep.ContentDuplicate, loc)
			f.Isolated = false // still parsed; excluded by UniqueFragments
			_ = first
		} else {
			seenHash[f.ContentSHA256] = f
		}
	}
	return rep
}

// UniqueFragments returns first occurrence of each content digest in fragment
// number/path order.  Repeated scans therefore produce the same index.
func UniqueFragments(frags []*Fragment, seq SequenceReport) []*Fragment {
	dupLocs := map[string]bool{}
	for _, l := range seq.ContentDuplicate {
		dupLocs[l.Path] = true
	}
	out := make([]*Fragment, 0, len(frags))
	for _, f := range frags {
		if f == nil || f.Isolated || dupLocs[f.Path] {
			continue
		}
		out = append(out, f)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].TrackID != out[j].TrackID {
			return out[i].TrackID < out[j].TrackID
		}
		return out[i].Sequence < out[j].Sequence
	})
	return out
}
