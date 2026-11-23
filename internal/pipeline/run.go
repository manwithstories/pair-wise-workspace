package pipeline

import (
	"errors"
	"fmt"

	"github.com/pion/rtp"

	"mediad/internal/audio"
	"mediad/internal/clock"
	"mediad/internal/color"
	"mediad/internal/fanin"
	"mediad/internal/fanout"
	"mediad/internal/index"
	"mediad/internal/report"
	mrtp "mediad/internal/rtp"
	"mediad/internal/stream"
)

// ErrUnrecovered marks runs that finished with unrecovered corrupt ranges.
var ErrUnrecovered = errors.New("unrecovered corrupt ranges present")

// auSpan locates one access unit's packets in the RTP sequence space.
type auSpan struct {
	first uint16
	count int
}

func (s auSpan) contains(seq uint16) bool {
	d := seq - s.first
	return int(d) < s.count
}

// Run executes the full delivery pipeline and returns aggregated metrics.
// Faults propagate across parse, recovery and index stages as located
// errors; unrecovered corrupt ranges yield ErrUnrecovered.
func Run(cfg Config) (report.Metrics, error) {
	var m report.Metrics
	if err := cfg.Validate(); err != nil {
		return m, err
	}
	src := Generate(cfg)

	// Stage 1: parse the elementary stream into access units.
	units, parseRanges := stream.Parse(stream.CodecH264, src.Stream)
	for _, r := range parseRanges {
		m.Faults = append(m.Faults, fmt.Sprintf("parse: corrupt range [%d,%d): %s", r.Start, r.End, r.Reason))
	}

	// Stage 2: packetize frames into RTP.
	var pkts []rtp.Packet
	seq := uint16(1)
	spans := make([]auSpan, len(src.Frames))
	for i, f := range src.Frames {
		spans[i] = auSpan{first: seq}
		ps := mrtp.Packetize(f.Payload, 0x15852, &seq, f.TS, 1200)
		spans[i].count = len(ps)
		pkts = append(pkts, ps...)
	}

	// Stage 3: fan out over independent per-rung channels. With
	// Unrecoverable set, the sender buffer has already dropped the lost
	// packets, so retransmission cannot fill the gaps.
	recoverySrc := pkts
	if cfg.Unrecoverable && cfg.LossBurst > 0 {
		start, blen := fanout.BurstWindow(len(pkts), 0, cfg.LossBurst)
		dropped := map[uint16]bool{}
		for _, p := range pkts[start : start+blen] {
			dropped[p.SequenceNumber] = true
		}
		recoverySrc = nil
		for _, p := range pkts {
			if !dropped[p.SequenceNumber] {
				recoverySrc = append(recoverySrc, p)
			}
		}
	}
	layers := fanout.FanOut(pkts, fanout.Ladder(6_000_000, cfg.Rates), cfg.LossBurst, recoverySrc)

	// Stage 4: per-rung AU completeness and loss-interval classification.
	received := make([]map[uint16]bool, len(layers))
	for li, lr := range layers {
		received[li] = map[uint16]bool{}
		for _, p := range lr.Packets {
			received[li][p.SequenceNumber] = true
		}
	}
	complete := func(li, au int) bool {
		for k := 0; k < spans[au].count; k++ {
			if !received[li][spans[au].first+uint16(k)] {
				return false
			}
		}
		return true
	}
	hadLoss := func(au int) bool {
		for k := 0; k < spans[au].count; k++ {
			if layers[0].LostSeqs[spans[au].first+uint16(k)] {
				return true
			}
		}
		return false
	}
	var recoveredRanges, unrecoveredRanges int
	for au := 0; au < len(spans); {
		if !hadLoss(au) {
			au++
			continue
		}
		end := au
		recovered := true
		for end < len(spans) && hadLoss(end) {
			if !complete(0, end) {
				recovered = false
			}
			end++
		}
		if recovered {
			recoveredRanges++
			m.Faults = append(m.Faults, fmt.Sprintf("rtp: loss burst over AU [%d,%d) recovered by retransmission", au, end))
		} else {
			unrecoveredRanges++
			m.Faults = append(m.Faults, fmt.Sprintf("rtp: unrecovered loss over AU [%d,%d); safe range: resync at next keyframe", au, end))
		}
		au = end
	}
	unrecoveredRanges += len(parseRanges)

	// Stage 5: repair rung-0 timestamps onto the unified clock.
	var samples []clock.Sample
	for au := range spans {
		if complete(0, au) {
			samples = append(samples, clock.Sample{Seq: uint64(au), TS: src.Frames[au].TS, Keyframe: src.Frames[au].Key})
		}
	}
	fixed, clockFaults, segs := clock.Repair(samples, clock.RepairConfig{
		NominalDelta: frameTicks,
		MaxGap:       4 * clock.Timescale,
		Trusted:      func(s uint64) bool { return src.Trusted[s] },
	})
	for _, f := range clockFaults {
		m.Faults = append(m.Faults, "clock: "+f.String())
	}
	for _, s := range segs {
		if s.Recovered {
			m.RecoveredSegments++
		}
	}

	// Stage 6: rebuild and validate the random-access index.
	idx, idxMism := index.Rebuild(units, fixed)
	idxMism = append(idxMism, index.Validate(idx, units)...)
	for _, mm := range idxMism {
		m.Faults = append(m.Faults, "index: "+mm.String())
	}
	m.IndexEntries = len(idx.Entries)
	m.IndexDigest = idx.Digest()
	syncTable, err := index.ExportSyncTable(idx, frameTicks)
	if err != nil {
		m.Faults = append(m.Faults, "index: sync table export: "+err.Error())
	} else {
		m.SyncTableBytes = len(syncTable)
	}

	// Stage 7: audio layout and color metadata checks.
	norm, audioIssues := audio.Validate(src.Audio)
	for _, i := range audioIssues {
		m.AudioIssues = append(m.AudioIssues, i.String())
	}
	_, colorConflicts := color.Resolve(src.CameraColor, src.BitstreamColor)
	for _, c := range colorConflicts {
		m.ColorConflicts = append(m.ColorConflicts, c.String())
	}
	m.SyncErrorMS = report.TicksToMS(norm.OffsetTicks) // video anchors at 0

	// Stage 8: fan-in boundary cross-check on the shared clock.
	timeBySeq := map[uint64]int64{}
	for _, f := range fixed {
		timeBySeq[f.Seq] = f.Time
	}
	perRung := map[string][]fanin.Boundary{}
	for li, lr := range layers {
		for au := range spans {
			if src.Frames[au].Key && complete(li, au) {
				perRung[lr.Rung.Name] = append(perRung[lr.Rung.Name],
					fanin.Boundary{Rung: lr.Rung.Name, Seq: uint64(au), Time: timeBySeq[uint64(au)]})
			}
		}
	}
	m.BoundaryMismatches = len(fanin.Check(perRung, 0))

	// Stage 9: aggregate recovery and latency metrics.
	var lost, recovered int
	var lat []float64
	for _, lr := range layers {
		lost += lr.Stats.Lost
		recovered += lr.Stats.Recovered
		lat = append(lat, lr.Stats.LatencyMS...)
	}
	m.LossRecoveryRate = 1
	if lost > 0 {
		m.LossRecoveryRate = float64(recovered) / float64(lost)
	}
	m.P95LatencyMS = report.P95(lat)
	m.CorruptedRanges = recoveredRanges + unrecoveredRanges
	m.UnrecoveredRanges = unrecoveredRanges

	if m.UnrecoveredRanges > 0 {
		return m, ErrUnrecovered
	}
	return m, nil
}
