package mp4

import (
	"bytes"
	"fmt"
	"io"

	"mediasim/internal/clock"
)

// resolveTrack turns the decoded tables into samples, keeping the valid
// prefix and marking every gap.
func (r *fileResolver) resolveTrack(td *trackDraft) Track {
	t := td.Track
	t.Samples = nil
	if td.Timescale == 0 {
		t.Timescale = 1
	}
	if !td.hasStsz && !td.hasStco && !td.hasStsc {
		// No byte-layout tables at all: the file is declared but unindexed.
		r.info.Indexed = false
		r.info.Gaps = append(r.info.Gaps, Gap{
			Kind: GapMissingIndex, TrackID: t.ID,
			Samples:  [2]int{0, sttsTotal(td.stts)},
			Recovery: clock.RecoveredResync,
			Detail:   "stsz/stco/stsc absent; bytes must be recovered by scanning",
		})
		return t
	}
	if !td.hasStsz || !td.hasStco || !td.hasStsc {
		r.info.Gaps = append(r.info.Gaps, Gap{
			Kind: GapTruncatedTable, TrackID: t.ID,
			Bytes:    [2]int64{td.stblOff, td.stblOff + td.stblAvail},
			Recovery: clock.RecoveredPrefix, Detail: "sample table is incomplete",
		})
	}
	count := int(td.stsz.count)
	if count < 0 || count > 1<<20 {
		r.info.Gaps = append(r.info.Gaps, Gap{
			Kind: GapOutOfRange, TrackID: t.ID, Bytes: [2]int64{td.stblOff, td.stblOff + td.stblAvail},
			Recovery: clock.RecoveredPrefix, Detail: fmt.Sprintf("declared sample count %d out of range", count),
		})
		count = 0
	}
	t.DeclaredSamples = count
	if !td.stsz.complete || !td.stco.complete || !td.stsc.complete {
		have := len(td.stsz.sizes)
		if td.stsz.sampleSize != 0 {
			have = count
		}
		r.info.Gaps = append(r.info.Gaps, Gap{
			Kind: GapTruncatedTable, TrackID: t.ID, Samples: [2]int{min(have, count), count},
			Bytes:    [2]int64{td.stblOff, td.stblOff + td.stblAvail},
			Recovery: clock.RecoveredPrefix, Detail: "sample table truncated; keeping valid prefix",
		})
	}

	// Map chunks to samples via stsc, then resolve byte offsets.
	offsets, sizes := chunkOffsets(td.stsz, td.stco, td.stsc)
	if len(offsets) > count {
		offsets = offsets[:count]
		sizes = sizes[:count]
	}
	if len(offsets) < count && td.stsz.complete && td.stco.complete && td.stsc.complete {
		r.info.Gaps = append(r.info.Gaps, Gap{
			Kind: GapTableMismatch, TrackID: t.ID, Samples: [2]int{len(offsets), count},
			Recovery: clock.RecoveredPrefix, Detail: "chunk map resolves fewer samples than stsz declares",
		})
	}

	dts, durations := expandStts(td.stts, len(offsets))
	syncSet := map[int]bool{}
	if td.wantSync {
		for _, n := range td.stss.numbers {
			syncSet[int(n)-1] = true
		}
		if len(td.stss.numbers) == 0 {
			r.info.Gaps = append(r.info.Gaps, Gap{
				Kind: GapTruncatedTable, TrackID: t.ID, Samples: [2]int{0, len(offsets)},
				Bytes:    [2]int64{td.stblOff, td.stblOff + td.stblAvail},
				Recovery: clock.RecoveredPrefix, Detail: "stss absent; sync flags unknown",
			})
		}
	}

	firstBad := -1
	for i := range offsets {
		s := Sample{Index: i, Offset: offsets[i], Size: sizes[i], DTS: dts[i], Duration: durations[i], Valid: true}
		s.Sync = !td.wantSync || syncSet[i]
		if reason := r.sampleProblem(s, t); reason != "" {
			s.Valid = false
			s.Reason = reason
			if firstBad < 0 {
				firstBad = i
			}
		}
		t.Samples = append(t.Samples, s)
	}
	if firstBad >= 0 {
		end := firstBad
		for end < len(t.Samples) && !t.Samples[end].Valid {
			end++
		}
		byteStart := offsets[firstBad]
		byteEnd := offsets[end-1] + int64(sizes[end-1])
		kind, rec := classifyBadRun(t.Samples[firstBad:end], r.info.Size)
		r.info.Gaps = append(r.info.Gaps, Gap{
			Kind: kind, TrackID: t.ID, Samples: [2]int{firstBad, end},
			Bytes: [2]int64{byteStart, byteEnd}, Recovery: rec,
			Detail: t.Samples[firstBad].Reason,
		})
	}
	if td.hasStts && sttsTotal(td.stts) != len(t.Samples) && td.stts.complete && len(t.Samples) > 0 {
		r.info.Gaps = append(r.info.Gaps, Gap{
			Kind: GapTableMismatch, TrackID: t.ID, Samples: [2]int{len(t.Samples), sttsTotal(td.stts)},
			Recovery: clock.RecoveredPrefix, Detail: "stts sample count disagrees with resolved samples",
		})
	}
	return t
}

// sampleProblem checks that the sample's bytes exist inside some mdat and
// carry the synthetic signature. It is deliberately strict so the corrupt
// fixture is detected by structure alone.
func (r *fileResolver) sampleProblem(s Sample, t Track) string {
	end := s.Offset + int64(s.Size)
	if s.Size == 0 {
		return "zero-size sample"
	}
	if s.Offset < 0 || end < s.Offset {
		return "negative or overflowing byte range"
	}
	inside := false
	for _, m := range r.info.Mdat {
		if s.Offset >= m[0] && end <= m[1] {
			inside = true
			break
		}
	}
	if !inside {
		return "byte range outside mdat or beyond EOF"
	}
	var sig [4]byte
	if _, err := r.f.ReadAt(sig[:], s.Offset); err != nil {
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return "truncated sample bytes"
		}
		return "unreadable sample bytes"
	}
	if r.info.Synthetic {
		// Every sample in our own containers must carry the MSMP header; a
		// mismatch is a corrupt sample even though its bytes exist.
		if !bytes.Equal(sig[:], sampleMagic) {
			return "corrupt sample signature"
		}
	}
	return ""
}
