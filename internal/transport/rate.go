package transport

import (
	"fmt"
	"sort"

	"github.com/pairwise/relay/internal/media"
)

// BandwidthPoint is one observed available-bitrate sample.
type BandwidthPoint struct {
	AtMS int64 `json:"at_ms"`
	Kbps int64 `json:"kbps"`
}

// BandwidthTrace is a time-ordered step-function bandwidth trajectory.
type BandwidthTrace []BandwidthPoint

// Normalize sorts points and drops duplicate timestamps while retaining the
// last observation at each timestamp.
func (t BandwidthTrace) Normalize() BandwidthTrace {
	if len(t) == 0 {
		return nil
	}
	out := append(BandwidthTrace(nil), t...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].AtMS < out[j].AtMS })
	dedup := out[:0]
	for i, p := range out {
		if i+1 < len(out) && out[i+1].AtMS == p.AtMS {
			continue
		}
		dedup = append(dedup, p)
	}
	return append(BandwidthTrace(nil), dedup...)
}

// KbpsAt returns the held bandwidth observation at time ms.
func (t BandwidthTrace) KbpsAt(ms int64) int64 {
	if len(t) == 0 {
		return 0
	}
	if ms <= t[0].AtMS {
		return t[0].Kbps
	}
	prev := t[0].Kbps
	for _, p := range t {
		if p.AtMS > ms {
			return prev
		}
		prev = p.Kbps
	}
	return prev
}

// RecoveryBudget bounds deterministic refetch simulation.
type RecoveryBudget struct {
	// TotalMillis is the global recovery wall/network budget.
	TotalMillis int64 `json:"total_millis"`
	// RTTMillis is charged for every refetch attempt.
	RTTMillis int64 `json:"rtt_millis"`
	// MaxAttempts caps attempts for one missing packet.
	MaxAttempts int `json:"max_attempts"`
}

// RecoveryItem is a missing packet and its on-wire size.
type RecoveryItem struct {
	ID    PacketID `json:"id"`
	Bytes int64    `json:"bytes"`
	// Unavailable models all known sources returning a permanent miss.
	Unavailable bool `json:"unavailable,omitempty"`
}

// RecoveryDecision explains whether one item fits the budget.
type RecoveryDecision struct {
	ID        PacketID `json:"id"`
	Attempts  int      `json:"attempts"`
	StartMS   int64    `json:"start_ms"`
	FinishMS  int64    `json:"finish_ms"`
	WireMS    int64    `json:"wire_ms"`
	Recovered bool     `json:"recovered"`
	Reason    string   `json:"reason,omitempty"`
}

// RecoveryReport is the result of planning all missing packets.
type RecoveryReport struct {
	Decisions []RecoveryDecision `json:"decisions"`
	Recovered []PacketID         `json:"recovered"`
	Failed    []RecoveryDecision `json:"failed"`
	ElapsedMS int64              `json:"elapsed_ms"`
	BudgetMS  int64              `json:"budget_ms"`
}

// PlanRecovery processes items in deterministic ID order and charges each
// refetch against one global budget using the changing bandwidth trace.
func PlanRecovery(items []RecoveryItem, trace BandwidthTrace, budget RecoveryBudget) RecoveryReport {
	trace = trace.Normalize()
	if budget.MaxAttempts <= 0 {
		budget.MaxAttempts = 1
	}
	report := RecoveryReport{BudgetMS: budget.TotalMillis}
	ordered := append([]RecoveryItem(nil), items...)
	sort.SliceStable(ordered, func(i, j int) bool {
		a, b := ordered[i].ID, ordered[j].ID
		if a.Rendition != b.Rendition {
			return a.Rendition < b.Rendition
		}
		return a.Sequence < b.Sequence
	})

	clockMS := int64(0)
	for _, item := range ordered {
		d := RecoveryDecision{ID: item.ID, StartMS: clockMS, Attempts: 1}
		if item.Bytes < 0 {
			d.Reason = "negative packet size"
			report.Failed = append(report.Failed, d)
			report.Decisions = append(report.Decisions, d)
			continue
		}
		if item.Unavailable {
			d.Reason = "all_sources_unavailable"
			report.Failed = append(report.Failed, d)
			report.Decisions = append(report.Decisions, d)
			continue
		}
		wireMS := TransferMillis(item.Bytes, clockMS+budget.RTTMillis, trace)
		finish := clockMS + budget.RTTMillis + wireMS
		d.WireMS = wireMS
		d.FinishMS = finish
		if finish > budget.TotalMillis {
			d.Reason = fmt.Sprintf("recovery budget exceeded: finish=%d budget=%d", finish, budget.TotalMillis)
			report.Failed = append(report.Failed, d)
			report.Decisions = append(report.Decisions, d)
			clockMS = budget.TotalMillis
			continue
		}
		d.Recovered = true
		d.Reason = "refetch_scheduled"
		clockMS = finish
		report.Recovered = append(report.Recovered, item.ID)
		report.Decisions = append(report.Decisions, d)
	}
	report.ElapsedMS = clockMS
	return report
}

// TransferMillis computes ceil(bytes*8 / bandwidth), following bandwidth
// changes at trace boundaries. Zero bandwidth blocks progress and returns a
// value larger than any practical budget.
func TransferMillis(bytes int64, startMS int64, trace BandwidthTrace) int64 {
	if bytes <= 0 {
		return 0
	}
	bits := bytes * 8
	if bits < bytes { // signed overflow guard for pathological inputs
		return 1<<62 - 1
	}
	cur := startMS
	const maxIterations = 1_000_000
	for i := 0; i < maxIterations; i++ {
		kbps := trace.KbpsAt(cur)
		if kbps <= 0 {
			// Advance to the next positive observation if one exists.
			next, ok := nextTraceTime(trace, cur)
			if !ok {
				return 1<<62 - 1
			}
			cur = next
			continue
		}
		next, hasNext := nextTraceTime(trace, cur)
		if !hasNext {
			whole := (bits + kbps - 1) / kbps // kbps numerically equals bits/ms
			return cur - startMS + whole
		}
		interval := next - cur
		canSend := kbps * interval
		if canSend < 0 {
			return 1<<62 - 1
		}
		if bits <= canSend {
			need := (bits + kbps - 1) / kbps
			return cur - startMS + need
		}
		bits -= canSend
		cur = next
	}
	return 1<<62 - 1
}

func nextTraceTime(trace BandwidthTrace, after int64) (int64, bool) {
	var next int64
	found := false
	for _, p := range trace {
		if p.AtMS > after && (!found || p.AtMS < next) {
			next = p.AtMS
			found = true
		}
	}
	return next, found
}

// Fault converts a failed recovery decision to the common fault model.
func (d RecoveryDecision) Fault() *media.Fault {
	code := media.CodeRecoveryBudget
	if d.Reason == "all_sources_unavailable" {
		code = media.CodeBurstLoss
	}
	return media.NewFault(code, media.FragmentID{Rendition: d.ID.Rendition, Segment: d.ID.Sequence}, -1, true, "%s", d.Reason)
}
