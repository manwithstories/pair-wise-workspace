package demo

import (
	"fmt"
	"sort"

	"github.com/clearliu/fieldcut/internal/clock"
	"github.com/clearliu/fieldcut/internal/index"
	"github.com/clearliu/fieldcut/internal/ingest"
	"github.com/clearliu/fieldcut/internal/stream"
	"github.com/clearliu/fieldcut/internal/timeline"
)

// finalize computes the terminal metrics and verifies seek invariants.
func finalize(reorder *ingest.ReorderBuffer, builder *index.Builder,
	engine *timeline.ReplayEngine, inQ, idxQ *stream.BoundedQueue,
	quarantine []index.QuarantineRecord) (Metrics, error) {

	idx, err := builder.Build()
	if err != nil {
		return Metrics{}, err
	}
	digest, err := idx.Digest()
	if err != nil {
		return Metrics{}, err
	}
	table, err := index.NewSeekTable(idx)
	if err != nil {
		return Metrics{}, err
	}
	for _, q := range quarantine {
		table.Quarantine(q.TrackID, q.Sequence, q.Reason)
	}
	// Deterministic seek verification: every indexed start must resolve.
	for _, e := range idx.Entries {
		if r := table.SeekExact(e.Start, e.TrackID); r.Miss {
			return Metrics{}, fmt.Errorf("%w: indexed entry not seekable", clock.ErrCheckpoint)
		}
	}
	_, misses := table.Stats()

	st := reorder.Stats()
	recovery := 1.0
	if st.GapPackets > 0 || st.RecoveredRTX > 0 {
		holes := st.GapPackets + st.RecoveredRTX + st.RecoveredLate
		if holes > 0 {
			recovery = float64(st.RecoveredRTX+st.RecoveredLate) / float64(holes)
		}
	}
	replays, _ := engine.Stats()

	committed := engine.Committed()
	var untrusted int
	for _, ev := range committed {
		if ev.Untrusted {
			untrusted++
		}
	}
	return Metrics{
		AVSyncP95Ms:         avSyncP95(committed),
		PacketRecoveryRate:  round3(recovery),
		PipelineLatencyP95Ms: 0, // filled by stage latency in Run
		PeakBufferBytes:     peakOf(inQ, idxQ),
		LateReplayCount:     replays,
		IndexSeekMisses:     misses,
		IndexSHA256:         digest,
		Quarantined:         len(table.QuarantineList()),
		UntrustedEvents:     untrusted,
		ProcessedPackets:    st.Received,
		GapPackets:          st.GapPackets,
	}, nil
}

func peakOf(qs ...*stream.BoundedQueue) int {
	var peak int
	for _, q := range qs {
		if p := q.Snapshot().PeakBytes; p > peak {
			peak = p
		}
	}
	return peak
}

// avSyncP95 measures the spread of frame starts around their ideal
// 40ms grid: a perfectly repaired single track reports 0ms.
func avSyncP95(events []*timeline.Event) int {
	if len(events) == 0 {
		return 0
	}
	var deltas []int64
	for _, ev := range events {
		if ev.Kind == timeline.KindGap {
			continue
		}
		ideal := (ev.Start / clock.Ticks(3600)) * clock.Ticks(3600)
		d := ev.Start - ideal
		if d < 0 {
			d = -d
		}
		deltas = append(deltas, d.Milliseconds())
	}
	if len(deltas) == 0 {
		return 0
	}
	sort.Slice(deltas, func(i, j int) bool { return deltas[i] < deltas[j] })
	rank := len(deltas) * 95 / 100
	if rank >= len(deltas) {
		rank = len(deltas) - 1
	}
	return int(deltas[rank])
}

func round3(v float64) float64 {
	return float64(int64(v*1000+0.5)) / 1000
}
