package ts

import (
	"sort"

	"tsqc/internal/clock"
)

func (d *Demux) Flush() []AccessUnit {
	pids := make([]uint16, 0, len(d.pes))
	for pid := range d.pes {
		pids = append(pids, pid)
	}
	sort.Slice(pids, func(i, j int) bool { return pids[i] < pids[j] })
	var out []AccessUnit
	for _, pid := range pids {
		acc := d.pes[pid]
		if len(acc.data) == 0 {
			continue
		}
		if au := d.finish(acc); au != nil {
			out = append(out, *au)
		}
		acc.data = nil
	}
	return out
}

// CheckSync compares the most recent audio and video PTS. It is separate from
// AU emission so the pipeline can attribute drift to an interval.
func (d *Demux) CheckSync() (int64, error) {
	if !d.videoSeen || !d.audioSeen {
		return 0, nil
	}
	delta := d.lastVideoPTS - d.lastAudioPTS
	if delta < 0 {
		delta = -delta
	}
	d.SyncSkew = delta
	if delta > clock.SyncToleranceTicks {
		d.syncFlagged = true
		return delta, clock.FatalError(clock.CodeSyncDrift, "ts.Demux", "A/V PTS skew %d ticks", delta)
	}
	return delta, nil
}

// TimelineFor exposes the per-PID unwrap state for verification.
func (d *Demux) TimelineFor(pid uint16) *clock.Timeline { return d.timeline[pid] }

// PendingPES reports buffered bytes per elementary PID. It lets a caller
// distinguish "no PES started yet" from "PES never boundaries".
func (d *Demux) PendingPES() map[uint16]int {
	out := make(map[uint16]int, len(d.pes))
	for pid, acc := range d.pes {
		out[pid] = len(acc.data)
	}
	return out
}
