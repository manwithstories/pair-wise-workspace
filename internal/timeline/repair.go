// Package timeline repairs frame timestamps against an explicit per-track
// clock. It classifies rollover, drift, duplicate, backwards and missing
// frames, records the evidence behind every inferred value and never rewrites
// the source SHA-256 recorded by the ingest stage.
package timeline

import (
	"fmt"

	"github.com/mediamend/mediamend/internal/ingest"
	"github.com/mediamend/mediamend/internal/model"
)

// RepairedFrame is one frame positioned on the unified 90kHz timeline.
type RepairedFrame struct {
	Frame        *ingest.Frame
	MasterTS     uint64 // corrected timestamp on the master clock
	Inferred     bool   // MasterTS was inferred rather than derived from input
	Evidence     string // human-readable justification of the correction
	RolloverSeen bool   // the raw device timestamp crossed a 32-bit boundary
}

// Evidence records why a timestamp was kept, inferred or rejected.
type Evidence struct {
	Seq      uint64 `json:"seq"`
	Kind     string `json:"kind"` // anchor|normal|rollover|backwards|duplicate|missing|checksum
	Reason   string `json:"reason"`
	RawTS    uint64 `json:"raw_ts"`
	MasterTS uint64 `json:"master_ts"`
}

// Report is the per-track repair result.
type Report struct {
	TrackID    string
	Clock      model.Clock
	Frames     []*RepairedFrame
	Evidence   []Evidence
	Faults     []model.Fault
	Duplicates uint64
	Backwards  uint64
	Missing    uint64
	Rollovers  uint64
	Inferred   uint64
}

// Repair re-times every frame of one track. The clock must carry an anchor
// whose sequence number exists in the input; the anchor frame is the single
// trusted point and is never adjusted. Frames keep their input order — the
// graph/pipeline stages decide what to do with duplicates and gaps.
func Repair(frames []*ingest.Frame, clock model.Clock, file string) *Report {
	rep := &Report{TrackID: clock.TrackID, Clock: clock}
	seen := make(map[uint64]int, len(frames))
	var prevRawUnwrapped uint64
	havePrev := false

	for _, fr := range frames {
		rf := &RepairedFrame{Frame: fr}
		expected := clock.ExpectedMaster(fr.Seq)
		loc := model.SourceLocation{File: file, Track: clock.TrackID, Seq: fr.Seq, Offset: fr.Offset}

		// 1. Checksum failures are carried forward, not silently rewritten.
		if !fr.ChecksumOK {
			rf.MasterTS = expected
			rf.Inferred = true
			rf.Evidence = "checksum invalid; timeline position inferred from sequence"
			rep.Inferred++
			rep.Evidence = append(rep.Evidence, Evidence{fr.Seq, "checksum", rf.Evidence, fr.DeviceTS, expected})
		} else if fr.Seq == clock.AnchorSeq {
			rf.MasterTS = clock.AnchorMasterTS
			rf.Evidence = "explicit anchor; trusted timestamp kept"
			rep.Evidence = append(rep.Evidence, Evidence{fr.Seq, "anchor", rf.Evidence, fr.DeviceTS, rf.MasterTS})
		} else {
			// 2. 32-bit rollover handling on the raw device clock.
			raw := fr.DeviceTS
			rollover := false
			if havePrev && model.IsRollover(prevRawUnwrapped, raw) {
				rollover = true
			}
			unwrapped := raw
			if havePrev {
				unwrapped = model.Unwrap(prevRawUnwrapped, raw)
			}

			// 3. Duplicate sequence number.
			if first, dup := seen[fr.Seq]; dup {
				rep.Duplicates++
				rf.MasterTS = rep.Frames[first].MasterTS
				rf.Inferred = true
				rf.Evidence = fmt.Sprintf("duplicate of seq %d; timestamp copied from first occurrence", fr.Seq)
				rep.Faults = append(rep.Faults, model.Fault{
					Class: model.FaultDuplicate, Severity: model.SeverityRecoverable,
					Location: loc, Message: rf.Evidence, Inferred: true, SourceCheck: fr.SourceSHA,
				})
				rep.Inferred++
				rep.Evidence = append(rep.Evidence, Evidence{fr.Seq, "duplicate", rf.Evidence, raw, rf.MasterTS})
			} else if havePrev && unwrapped < prevRawUnwrapped && !rollover {
				// 4. Genuine backwards jump (device clock reset / bad jump point).
				rep.Backwards++
				rf.MasterTS = expected
				rf.Inferred = true
				rf.Evidence = fmt.Sprintf("backwards device timestamp (%d -> %d); reset to sequence-derived value",
					prevRawUnwrapped, unwrapped)
				rep.Faults = append(rep.Faults, model.Fault{
					Class: model.FaultBackwards, Severity: model.SeverityRecoverable,
					Location: loc, Message: rf.Evidence,
					Expected: fmt.Sprintf(">= %d", prevRawUnwrapped), Actual: fmt.Sprintf("%d", unwrapped),
					Inferred: true, SourceCheck: fr.SourceSHA,
				})
				rep.Inferred++
				rep.Evidence = append(rep.Evidence, Evidence{fr.Seq, "backwards", rf.Evidence, raw, expected})
			} else {
				if rollover {
					rep.Rollovers++
					rf.RolloverSeen = true
					rep.Faults = append(rep.Faults, model.Fault{
						Class: model.FaultRollover, Severity: model.SeverityInfo,
						Location: loc,
						Message:  fmt.Sprintf("32-bit timestamp rollover unwrapped: raw=%d unwrapped=%d", raw, unwrapped),
						Actual:   fmt.Sprintf("%d", unwrapped), SourceCheck: fr.SourceSHA,
					})
				}
				// 5. Normal path: convert the device stamp, then correct drift
				// against the sequence-derived expectation.
				converted := clock.ToMaster(unwrapped-clock.AnchorDeviceTS) + clock.AnchorMasterTS
				rf.MasterTS, rf.Evidence = reconcile(converted, expected, raw, rollover)
				if rf.Inferred {
					rep.Inferred++
				}
				kind := "normal"
				if rollover {
					kind = "rollover"
				}
				rep.Evidence = append(rep.Evidence, Evidence{fr.Seq, kind, rf.Evidence, raw, rf.MasterTS})
			}
		}

		// 6. Missing-frame detection against the previous sequence number.
		if prior, ok := previousSeq(seen, fr.Seq); ok && fr.Seq > prior+1 {
			gap := fr.Seq - prior - 1
			rep.Missing += gap
			rep.Faults = append(rep.Faults, model.Fault{
				Class: model.FaultMissing, Severity: model.SeverityRecoverable,
				Location: loc,
				Message:  fmt.Sprintf("%d missing frame(s) between seq %d and %d", gap, prior, fr.Seq),
				Expected: fmt.Sprintf("seq %d", prior+1), Actual: fmt.Sprintf("seq %d", fr.Seq),
				SourceCheck: fr.SourceSHA,
			})
		}

		seen[fr.Seq] = len(rep.Frames)
		rep.Frames = append(rep.Frames, rf)
		if fr.ChecksumOK {
			if havePrev {
				prevRawUnwrapped = model.Unwrap(prevRawUnwrapped, fr.DeviceTS)
			} else {
				prevRawUnwrapped = fr.DeviceTS
			}
			havePrev = true
		}
	}
	return rep
}

// reconcile chooses between the converted device timestamp and the
// sequence-derived expectation. Drift within one master frame is kept; larger
// divergence is corrected to the expectation and marked inferred.
func reconcile(converted, expected, raw uint64, rollover bool) (uint64, string) {
	diff := int64(converted) - int64(expected)
	if diff < 0 {
		diff = -diff
	}
	if diff <= int64(model.MasterRate)/60 { // within one ~16.7ms frame
		return converted, "device timestamp kept; drift within one frame"
	}
	return expected, fmt.Sprintf("drift %d master ticks exceeds one frame; corrected from sequence", diff)
}

func previousSeq(seen map[uint64]int, current uint64) (uint64, bool) {
	best := uint64(0)
	found := false
	for seq := range seen {
		if seq < current && (!found || seq > best) {
			best, found = seq, true
		}
	}
	return best, found
}
