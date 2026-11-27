// Package fanout runs the per-branch pipeline in parallel and merges
// keyframe boundaries on the shared clock.
package fanout

import (
	"sort"
	"sync"

	"tscheck/internal/analyze"
	"tscheck/internal/clock"
	"tscheck/internal/model"
	"tscheck/internal/ts"
)

// MaxBranches bounds the number of concurrent bitrate branches.
const MaxBranches = 3

// Branch is one bitrate ladder input.
type Branch struct {
	Name string
	Data []byte
}

// BranchResult aggregates one branch's analysis. Faults are sorted by
// offset so downstream stages can link them to analysis results by
// position.
type BranchResult struct {
	Name    string
	Packets int
	Tracks  []analyze.TrackStats
	GOP     analyze.GOPStats
	Drift   float64
	Faults  []model.Fault
	Trusted []ts.PacketInfo // packets that passed sync and continuity checks
}

// Process runs the full pipeline on one branch: scan, PSI, clock
// unwrap, structure and GOP analysis.
func Process(b Branch) *BranchResult {
	res := &BranchResult{Name: b.Name}
	pkts, rep := ts.NewScanner().Scan(b.Data)
	res.Packets = rep.Packets
	res.Faults = append(res.Faults, rep.Faults...)

	psi := ts.NewPSITracker()
	clk := clock.NewClock()
	drift := clock.NewDriftEstimator(500)
	str := analyze.NewStructure()
	gop := analyze.NewGOPTracker()
	assemblers := map[uint16]*analyze.PESAssembler{}
	videoPIDs := map[uint16]bool{}
	var lastPCR model.Timestamp

	faultAt := map[int64]bool{}
	for _, f := range rep.Faults {
		faultAt[f.Offset] = true
	}
	for _, pi := range pkts {
		if !faultAt[pi.Offset] {
			res.Trusted = append(res.Trusted, pi)
		}
		res.Faults = append(res.Faults, psi.Feed(pi)...)
		str.SetStreams(psi.Streams())
		for _, st := range psi.Streams() {
			if st.StreamType == 0x1B {
				videoPIDs[st.PID] = true
			}
		}
		str.AddPacket(pi)
		if pi.HasPCR {
			lastPCR = clk.ObservePCR(pi.PCRBase, pi.PCRExt)
		}
		if !videoPIDs[pi.PID] {
			continue
		}
		asm := assemblers[pi.PID]
		if asm == nil {
			asm = analyze.NewPESAssembler(pi.PID)
			assemblers[pi.PID] = asm
		}
		pes, fault := asm.Feed(pi)
		if fault != nil {
			res.Faults = append(res.Faults, *fault)
		}
		if pes == nil {
			continue
		}
		if !pes.HasPTS {
			continue
		}
		pts := clk.UnwrapPTS(pes.PTSRaw)
		str.AddPTS(pi.PID, pts)
		if pts.Anchored && lastPCR.Anchored {
			drift.Observe(lastPCR.Ticks, pts.Ticks)
		}
		gop.AddFrame(pts, analyze.ContainsIDR(pes.ES))
	}
	for _, asm := range assemblers {
		if f := asm.Flush(); f != nil {
			res.Faults = append(res.Faults, *f)
		}
	}
	res.Tracks = str.Report()
	res.GOP = gop.Stats()
	res.Drift = drift.Drift()
	sort.Slice(res.Faults, func(i, j int) bool { return res.Faults[i].Offset < res.Faults[j].Offset })
	return res
}

// Run processes branches in parallel, bounded to MaxBranches workers.
func Run(branches []Branch) []*BranchResult {
	if len(branches) > MaxBranches {
		branches = branches[:MaxBranches]
	}
	results := make([]*BranchResult, len(branches))
	sem := make(chan struct{}, MaxBranches)
	var wg sync.WaitGroup
	for i, b := range branches {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = Process(b)
		}()
	}
	wg.Wait()
	return results
}
