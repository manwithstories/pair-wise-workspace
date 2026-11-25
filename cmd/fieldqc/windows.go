package main

import (
	"context"
	"fmt"
	"math"
	"time"

	pionrtp "github.com/pion/rtp"

	"fieldqc/internal/fault"
	"fieldqc/internal/fixture"
	"fieldqc/internal/report"
	fqrtp "fieldqc/internal/rtp"
	"fieldqc/internal/stream"
)

// memBudget bounds total in-flight pipeline memory (需求七).
const memBudget = 256 << 20 // 256 MiB

// windowRes carries one media window's packets, the gaps its repair
// stage newly declared, and its processing time. vGaps/aGaps are owned
// by the item once handed downstream (the repairers stay with repair).
type windowRes struct {
	vpkts, apkts []*pionrtp.Packet
	vGaps, aGaps []fqrtp.Gap
	procMs       float64
}

// centerMeter re-seats every emitted packet — including recovered
// stragglers a later hop finally emits — into its true media window by
// timestamp, then averages raw packet-center times. Measuring only the
// packets emitted during the current window would drop the tail held by
// the reorder window and systematically bias the A/V center.
type centerMeter struct {
	perWin    uint32 // global media indices per window
	ticks     uint32 // timestamp ticks per index (frame / 20 ms packet)
	rate      uint32 // RTP clock rate
	origin    uint32 // timestamp origin removed to recover the index
	nWin      int
	sumCenter []uint64 // sum of raw center timestamps per window
	count     []int
}

func newCenterMeter(perWin, ticks, rate, origin uint32, nWin int) *centerMeter {
	return &centerMeter{
		perWin: perWin, ticks: ticks, rate: rate, origin: origin, nWin: nWin,
		sumCenter: make([]uint64, nWin), count: make([]int, nWin),
	}
}

// add folds emitted packets into their timestamp-resolved windows. The
// synthesized span stays below the uint32 timestamp wrap (2 h), so raw
// timestamps are summed directly; index recovery uses modular subtract.
func (m *centerMeter) add(pkts []*pionrtp.Packet) {
	for _, p := range pkts {
		idx := (p.Timestamp - m.origin) / m.ticks
		w := int(idx / m.perWin)
		if w < 0 || w >= m.nWin {
			continue
		}
		m.sumCenter[w] += uint64(p.Timestamp) + uint64(m.ticks)/2 // packet center
		m.count[w]++
	}
}

// centerSec is the mean packet-center presentation time for window w.
func (m *centerMeter) centerSec(w int) (float64, bool) {
	if w < 0 || w >= m.nWin || m.count[w] == 0 {
		return 0, false
	}
	return float64(m.sumCenter[w]) / float64(m.count[w]) / float64(m.rate), true
}

func gapSpan(track string, g fqrtp.Gap) fault.Span {
	return fault.Span{Track: track, Code: fault.CodeRTPUnrecoverable,
		Start: g.Start, End: g.End,
		Detail: fmt.Sprintf("seq %d-%d unrecoverable", g.StartSeq, g.EndSeq)}
}

// runWindows streams cfg-sized media windows through the bounded
// pipeline: a repair stage is the single owner of the RTP repairers,
// reorders/recovers packets, re-seats them into the sync meters and
// attaches newly declared gaps to each item; a diagnose stage turns
// those gaps into fault spans. It returns combined RTP stats, per-window
// A/V sync errors (ms) and the P95 per-window processing time (ms).
func runWindows(ctx context.Context, cfg config, name string, model *fault.Model) (fqrtp.Stats, []float64, float64, error) {
	vg := fixture.NewRTPGen(cfg.seed, 3600, cfg.loss, 1) // video: 25 fps @ 90 kHz
	ag := fixture.NewRTPGen(cfg.seed+1, 960, cfg.loss, 1) // audio: 20 ms packets @ 48 kHz
	ag.Offset = 240                                      // 5 ms constant A/V origin offset
	winV := int(cfg.window.Seconds() * 25)
	winA := int(cfg.window.Seconds() * 50)
	nWin := cfg.hours * 3600 * 25 / winV
	if name == "rtp-burst" {
		vg.BurstAt, vg.BurstLen = nWin/3*winV, 30 // 1.2 s of video gone for good
	}
	vrep, arep := fqrtp.NewRepairer(8, 90000), fqrtp.NewRepairer(8, 48000)
	vm := newCenterMeter(uint32(winV), 3600, 90000, 0, nWin)
	am := newCenterMeter(uint32(winA), 960, 48000, ag.Offset, nWin)

	src := make(chan stream.Item)
	go func() {
		defer close(src)
		for w := 0; w < nWin; w++ {
			it := stream.Item{Window: w, Size: int64((winV + winA) * 128),
				Payload: &windowRes{vpkts: vg.NextWindow(winV), apkts: ag.NextWindow(winA)}}
			select {
			case src <- it:
			case <-ctx.Done():
				return
			}
		}
	}()

	// Gap cursors are touched only by the repair worker (the sole owner
	// of the repairers), so Gaps() never races Push()/Flush().
	nVGaps, nAGaps := 0, 0
	freshGaps := func(rep *fqrtp.Repairer, seen *int) []fqrtp.Gap {
		all := rep.Gaps() // returns an immutable fresh copy
		out := all[*seen:]
		*seen = len(all)
		return out
	}

	repair := func(ctx context.Context, in <-chan stream.Item, out chan<- stream.Item) error {
		for {
			select {
			case <-ctx.Done():
				return stream.ErrAborted
			case it, ok := <-in:
				if !ok {
					return nil
				}
				w := it.Payload.(*windowRes)
				t0 := time.Now()
				for _, p := range w.vpkts {
					vrep.Push(p)
				}
				for _, p := range w.apkts {
					arep.Push(p)
				}
				// Re-seat everything emitted into its timestamp window.
				vm.add(vrep.Take())
				am.add(arep.Take())
				w.vGaps = freshGaps(vrep, &nVGaps)
				w.aGaps = freshGaps(arep, &nAGaps)
				w.procMs = float64(time.Since(t0).Microseconds()) / 1000
				select {
				case out <- it:
				case <-ctx.Done():
					return stream.ErrAborted
				}
			}
		}
	}

	var durs []float64
	diagnoseStage := func(ctx context.Context, in <-chan stream.Item, out chan<- stream.Item) error {
		for {
			select {
			case <-ctx.Done():
				return stream.ErrAborted
			case it, ok := <-in:
				if !ok {
					return nil
				}
				w := it.Payload.(*windowRes)
				durs = append(durs, w.procMs)
				for _, g := range w.vGaps {
					model.Add(gapSpan("card0/cama-v", g))
				}
				for _, g := range w.aGaps {
					model.Add(gapSpan("card0/cama-a", g))
				}
				select {
				case out <- it:
				case <-ctx.Done():
					return stream.ErrAborted
				}
			}
		}
	}

	p := stream.New(4, memBudget)
	if err := p.Run(ctx, src, repair, diagnoseStage); err != nil {
		return fqrtp.Stats{}, nil, 0, err
	}
	// Run has joined every stage: safe to flush and read repairers/model.
	vrep.Flush()
	arep.Flush()
	vm.add(vrep.Take())
	am.add(arep.Take())
	for _, g := range vrep.Gaps()[nVGaps:] {
		model.Add(gapSpan("card0/cama-v", g))
	}
	for _, g := range arep.Gaps()[nAGaps:] {
		model.Add(gapSpan("card0/cama-a", g))
	}

	syncs := make([]float64, 0, nWin)
	for w := 0; w < nWin; w++ {
		if vc, vok := vm.centerSec(w); vok {
			if ac, aok := am.centerSec(w); aok {
				syncs = append(syncs, math.Abs(vc-ac)*1000)
			}
		}
	}
	stats := vrep.Stats()
	stats.Add(arep.Stats())
	return stats, syncs, report.P95(durs), nil
}
