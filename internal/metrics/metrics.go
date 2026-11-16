// Package metrics folds the observations of every layer — signaling, peers,
// forwarder and the RTP analyser — into one report with stable JSON field
// names.
//
// Values are copied in, never derived twice: the report's packet counts are
// the forwarder's and analyser's counters verbatim, so a script can trust that
// forwarded_packets + backpressure + sender loss accounts for the source
// count.
package metrics

import (
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/manwithstories/pair-wise-workspace/internal/forwarder"
	rtpx "github.com/manwithstories/pair-wise-workspace/internal/rtp"
	"github.com/manwithstories/pair-wise-workspace/internal/signaling"
)

// Report is the single JSON object the CLI prints. Field names are part of
// the contract and must not be renamed.
type Report struct {
	// Connection facts.
	CandidatePairs int      `json:"candidate_pairs"`
	LoopbackOnly   bool     `json:"loopback_only"`
	LocalAddr      string   `json:"local_addr,omitempty"`
	RemoteAddr     string   `json:"remote_addr,omitempty"`
	Candidates     []string `json:"candidates,omitempty"`
	Rejected       []string `json:"rejected,omitempty"`

	// Packet accounting.
	SourcePackets     int    `json:"source_packets"`
	ForwardedPackets  uint64 `json:"forwarded_packets"`
	BackpressureDrops uint64 `json:"backpressure_drops"`
	LostPackets       int64  `json:"lost_packets"`
	DuplicatePackets  uint64 `json:"duplicate_packets"`
	ReorderedPackets  uint64 `json:"reordered_packets"`
	LateOutsideWindow uint64 `json:"late_outside_window"`
	ReceivedPackets   uint64 `json:"received_packets"`
	UniquePackets     uint64 `json:"unique_packets"`

	// Jitter, in milliseconds.
	JitterAvgMs float64 `json:"jitter_avg_ms"`
	JitterMaxMs float64 `json:"jitter_max_ms"`

	// Wiring details.
	ClockRate     int    `json:"clock_rate"`
	ReorderWindow int    `json:"reorder_window"`
	SSRC          uint32 `json:"ssrc"`
	WrapCount     uint64 `json:"wrap_count"`

	// Signaling health, useful when a run is slow or fails.
	SignalingMessages int64 `json:"signaling_messages"`
	SignalingRejected int64 `json:"signaling_rejected"`
	SignalingSessions int64 `json:"signaling_sessions"`
	DurationMs        int64 `json:"duration_ms"`
	ExitCode          int   `json:"exit_code"`
}

// Inputs is everything the report needs, gathered by the command.
type Inputs struct {
	LocalAddr  string
	RemoteAddr string
	Candidates []string
	Rejected   []string
	Loopback   bool

	SourcePackets int
	StreamStats   rtpx.Summary
	Forwarder     forwarder.Stats
	Signaling     signaling.Stats

	Duration time.Duration
	ExitCode int
}

// Build assembles a Report. A zero-value stream summary yields zero jitter
// and zero counts rather than an error, so a failed run can still print a
// machine-readable report explaining itself.
func Build(in Inputs) Report {
	pairs := 0
	if in.LocalAddr != "" && in.RemoteAddr != "" {
		pairs = 1
	}
	return Report{
		CandidatePairs:    pairs,
		LoopbackOnly:      in.Loopback,
		LocalAddr:         in.LocalAddr,
		RemoteAddr:        in.RemoteAddr,
		Candidates:        in.Candidates,
		Rejected:          in.Rejected,
		SourcePackets:     in.SourcePackets,
		ForwardedPackets:  in.Forwarder.Forwarded,
		BackpressureDrops: in.Forwarder.Backpressure,
		LostPackets:       in.StreamStats.Lost,
		DuplicatePackets:  in.StreamStats.Duplicates,
		ReorderedPackets:  in.StreamStats.Reordered,
		LateOutsideWindow: in.StreamStats.LateOutside,
		ReceivedPackets:   in.StreamStats.Received,
		UniquePackets:     in.StreamStats.Unique,
		JitterAvgMs:       in.StreamStats.JitterAvgMs,
		JitterMaxMs:       in.StreamStats.JitterMaxMs,
		ClockRate:         in.StreamStats.ClockRate,
		ReorderWindow:     in.StreamStats.ReorderWindow,
		SSRC:              firstSSRC(in.StreamStats),
		WrapCount:         firstWraps(in.StreamStats),
		SignalingMessages: in.Signaling.MessagesForwarded,
		SignalingRejected: in.Signaling.MessagesRejected,
		SignalingSessions: in.Signaling.SessionsCreated,
		DurationMs:        in.Duration.Milliseconds(),
		ExitCode:          in.ExitCode,
	}
}

func firstSSRC(s rtpx.Summary) uint32 {
	if len(s.Streams) == 0 {
		return 0
	}
	return s.Streams[0].SSRC
}

func firstWraps(s rtpx.Summary) uint64 {
	var total uint64
	for _, st := range s.Streams {
		total += st.WrapCount
	}
	return total
}

// Encode writes the report as indented JSON followed by a newline.
func (r Report) Encode(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// Human renders the short readable summary printed before the JSON. It leads
// with the numbers the demo acceptance criteria name.
func (r Report) Human() string {
	return fmt.Sprintf(
		"candidate_pairs=%d\nforwarded_packets=%d\nlost_packets=%d\nduplicate_packets=%d\n"+
			"jitter_avg_ms=%.3f\njitter_max_ms=%.3f\nloopback_only=%t\nexit_code=%d\n",
		r.CandidatePairs, r.ForwardedPackets, r.LostPackets, r.DuplicatePackets,
		r.JitterAvgMs, r.JitterMaxMs, r.LoopbackOnly, r.ExitCode,
	)
}

// Thresholds describes what a demo run is expected to observe. A negative
// bound means "no constraint"; the CLI ignores those.
type Thresholds struct {
	MinForwarded uint64
	MaxForwarded uint64
	MinLost      int64
	MaxLost      int64
	MinDup       uint64
	MaxDup       uint64
	MaxJitterAvg float64
	RequirePairs bool
}

// Check returns the list of violated constraints. An empty slice means the
// report is acceptable; a non-empty one should map to exit code 4.
func (r Report) Check(t Thresholds) []string {
	var problems []string
	if t.RequirePairs && r.CandidatePairs != 1 {
		problems = append(problems, fmt.Sprintf("candidate_pairs=%d, want 1", r.CandidatePairs))
	}
	if !r.LoopbackOnly {
		problems = append(problems, "loopback_only=false")
	}
	if r.ForwardedPackets < t.MinForwarded {
		problems = append(problems, fmt.Sprintf("forwarded_packets=%d below %d", r.ForwardedPackets, t.MinForwarded))
	}
	if t.MaxForwarded > 0 && r.ForwardedPackets > t.MaxForwarded {
		problems = append(problems, fmt.Sprintf("forwarded_packets=%d above %d", r.ForwardedPackets, t.MaxForwarded))
	}
	if t.MaxLost > 0 && r.LostPackets > t.MaxLost {
		problems = append(problems, fmt.Sprintf("lost_packets=%d above %d", r.LostPackets, t.MaxLost))
	}
	if t.MinLost > 0 && r.LostPackets < t.MinLost {
		problems = append(problems, fmt.Sprintf("lost_packets=%d below %d", r.LostPackets, t.MinLost))
	}
	if r.DuplicatePackets < t.MinDup {
		problems = append(problems, fmt.Sprintf("duplicate_packets=%d below %d", r.DuplicatePackets, t.MinDup))
	}
	if t.MaxDup > 0 && r.DuplicatePackets > t.MaxDup {
		problems = append(problems, fmt.Sprintf("duplicate_packets=%d above %d", r.DuplicatePackets, t.MaxDup))
	}
	if t.MaxJitterAvg > 0 && r.JitterAvgMs > t.MaxJitterAvg {
		problems = append(problems, fmt.Sprintf("jitter_avg_ms=%.3f above %.3f", r.JitterAvgMs, t.MaxJitterAvg))
	}
	if r.JitterMaxMs < r.JitterAvgMs {
		problems = append(problems, fmt.Sprintf("jitter_max_ms=%.3f below jitter_avg_ms=%.3f", r.JitterMaxMs, r.JitterAvgMs))
	}
	return problems
}

// DefaultThresholds derives the acceptance envelope from the requested fault
// rates. The bounds are deliberately wide: they exist to catch a broken path
// (zero forwards, zero loss when loss was asked for, a wildly different
// duplicate count), not to pin a run to an exact number.
//
// A zero rate produces a zero floor, so a clean run is never failed for
// correctly reporting zero faults.
func DefaultThresholds(packets int, lossRate, dupRate float64) Thresholds {
	dropped := int64(float64(packets) * lossRate)
	dups := uint64(float64(packets) * dupRate * 0.5)
	return Thresholds{
		MinForwarded: uint64(float64(packets) * 0.97),
		MaxForwarded: uint64(packets) + dups + 2,
		MinLost:      dropped / 2,
		MaxLost:      dropped*2 + 2,
		MinDup:       dups,
		MaxDup:       dups * 3,
		MaxJitterAvg: 20,
		RequirePairs: true,
	}
}
