package main

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"mediapack/internal/dag"
	"mediapack/internal/hash"
	"mediapack/internal/index"
)

// printer writes the report with a single error check at the boundary, so
// that formatting code stays readable.
type printer struct{ w io.Writer }

func newPrinter(w io.Writer) *printer { return &printer{w: w} }

func (p *printer) Write(b []byte) (int, error) { return p.w.Write(b) }

func (p *printer) printf(format string, args ...any) {
	fmt.Fprintf(p.w, format, args...)
}

func (p *printer) line(format string, args ...any) {
	fmt.Fprintf(p.w, format+"\n", args...)
}

// printReport emits the fields the acceptance check reads, plus a compact
// table for operators.  The fixed field names are part of the contract.
func (rs *runState) printReport(p *printer) {
	p.line("status=ok")
	p.line("roundtrip_match_pct=%.2f", rs.roundTripPct())
	p.line("keyframe_hit_pct=%.2f", rs.keyframeHitPct())
	p.line("segments=%d", len(rs.reportManifest.Segments))
	for _, entry := range rs.reportManifest.Segments {
		p.line("segment_sha256=%s", entry.SHA256)
	}
	p.line("segment_ticks=%d", rs.opts.SegmentTick)
	p.line("segment_seconds=%s", rs.frameTicks)
	p.line("primary_rate=%d", rs.bundle.Rates[0].Value)
	p.line("representations=%d", len(rs.bundle.Rates))
	p.line("frames=%d", rs.bundle.FrameCount)
	p.line("audio=%s", rs.audioSummary())
	p.line("color=%s", rs.colorTriplet())
	p.line("manifest=%s", rs.manifestPath())
	p.line("nodes_done=%d", countDone(rs.state))
	p.line("resumed_nodes=%d", rs.resumedCount())
}

// resumedCount is the number of nodes this invocation reused from durable
// state instead of executing.  A fresh run reports zero.
func (rs *runState) resumedCount() int {
	if rs.run == nil {
		return 0
	}
	return len(rs.run.Resumed)
}

func (rs *runState) roundTripPct() float64 {
	art, err := readArtifact[mp4Artifact](rs, "mp4", rs.configHash())
	if err != nil || len(art) == 0 {
		return 0
	}
	total := 0.0
	for _, pct := range art {
		total += pct
	}
	return total / float64(len(art))
}

// keyframeHitPct is the fraction of primary keyframe boundaries that exist
// exactly in every representation's index.  A uniform fixture yields 100.
// keyframeHitPct is recomputed from the MP4 summaries, which are always
// available: it must be reported identically whether or not the index node
// was skipped on this invocation.
func (rs *runState) keyframeHitPct() float64 {
	reps, err := rs.representations()
	if err != nil || len(reps) == 0 {
		return 0
	}
	primary := reps[0].Keyframes.Keyframes
	hits, total := 0, 0
	for _, kf := range primary {
		total++
		all := true
		for _, r := range reps[1:] {
			found := false
			for _, other := range r.Keyframes.Keyframes {
				if other.Ticks.Equal(kf.Ticks) {
					found = true
					break
				}
			}
			if !found {
				all = false
				break
			}
		}
		if all {
			hits++
		}
	}
	if total == 0 {
		return 100
	}
	return float64(hits) / float64(total) * 100
}

func (rs *runState) audioSummary() string {
	art, err := readArtifact[audioArtifact](rs, "audio", rs.configHash())
	if err != nil {
		return "unknown"
	}
	return art.Summary
}

func (rs *runState) colorTriplet() string {
	art, err := readArtifact[colorArtifact](rs, "color", rs.configHash())
	if err != nil {
		return "unknown"
	}
	return art.Triplet
}

// printBlockedChain prints the earliest blocking dependency for operators.
// It reads persisted state, so it works even when the failing run itself had
// no in-memory result.
func printBlockedChain(p *printer, state *dag.State) {
	if state == nil {
		return
	}
	names := make([]string, 0, len(state.Nodes))
	for name := range state.Nodes {
		names = append(names, name)
	}
	sort.Strings(names)
	fe := dag.EarliestBlocked(state, names)
	if fe == nil {
		return
	}
	p.line("blocked_chain=%s", chainString(state, fe.Track))
}

// chainString renders the dependency path from the earliest blocker to the
// node that could not run.
func chainString(state *dag.State, node string) string {
	chain, err := dag.BlockedChain(state, node)
	if err != nil || len(chain) == 0 {
		return node
	}
	return strings.Join(chain, ">")
}

func countDone(state *dag.State) int {
	n := 0
	for _, rec := range state.Nodes {
		if rec.Status == dag.StatusDone {
			n++
		}
	}
	return n
}

var _ = hash.AllMatch
var _ = index.Coverage
