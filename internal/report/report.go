// Package report builds the recomputable capture report: identical field names
// for TCP and UDP transports, JSON serialization, text rendering and the
// acceptance-range verification used by the verify subcommand.
package report

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	rtppkg "github.com/example/rtpcap/internal/rtp"
	"github.com/example/rtpcap/internal/rtsp"
	"github.com/example/rtpcap/internal/scenario"
	"github.com/example/rtpcap/internal/store"
)

// Report is the single output schema shared by both transport modes.
type Report struct {
	Scenario           string               `json:"scenario"`
	Transport          string               `json:"transport"`
	StateTransitions   int                  `json:"state_transitions"`
	Transitions        []rtsp.Transition    `json:"transitions"`
	IllegalTransitions int                  `json:"illegal_transitions"`
	RTPReceived        int                  `json:"rtp_received"`
	RTPDuplicates      int                  `json:"rtp_duplicates"`
	RTPReordered       int                  `json:"rtp_reordered"`
	SeqGapSegments     int                  `json:"seq_gap_segments"`
	SeqGapPackets      int                  `json:"seq_gap_packets"`
	Gaps               []rtppkg.GapInterval `json:"seq_gaps"`
	MaxReorderDepth    int                  `json:"max_reorder_depth"`
	FramesReassembled  int                  `json:"frames_reassembled"`
	Bytes              int64                `json:"bytes"`
	FirstSeq           uint16               `json:"first_seq"`
	LastSeq            uint16               `json:"last_seq"`
	Reconnects         int                  `json:"reconnects"`
	ExitCode           int                  `json:"exit_code"`
	Error              string               `json:"error,omitempty"`
}

// Build assembles a report from the live capture components.
func Build(name, transport string, sm *rtsp.StateMachine, stats rtppkg.Stats,
	gaps []rtppkg.GapInterval, reconnects, exitCode int, errMsg string) Report {
	transitions := sm.Transitions()
	illegal := 0
	for _, tr := range transitions {
		if !tr.Legal {
			illegal++
		}
	}
	return Report{
		Scenario:           name,
		Transport:          transport,
		StateTransitions:   sm.LegalTransitionCount(),
		Transitions:        transitions,
		IllegalTransitions: illegal,
		RTPReceived:        stats.Received,
		RTPDuplicates:      stats.Duplicates,
		RTPReordered:       stats.Reordered,
		SeqGapSegments:     stats.GapSegments,
		SeqGapPackets:      stats.GapPackets,
		Gaps:               gaps,
		MaxReorderDepth:    stats.MaxReorderDepth,
		FramesReassembled:  stats.Frames,
		Bytes:              stats.Bytes,
		FirstSeq:           stats.FirstSeq,
		LastSeq:            stats.LastSeq,
		Reconnects:         reconnects,
		ExitCode:           exitCode,
		Error:              errMsg,
	}
}

// WriteJSON writes the report as indented JSON.
func (r Report) WriteJSON(path string) error {
	raw, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(raw, '\n'), 0o644)
}

// LoadJSON reads a report from disk.
func LoadJSON(path string) (*Report, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r Report
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// Text renders the human-readable terminal summary.
func (r Report) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "scenario            : %s (%s)\n", r.Scenario, r.Transport)
	fmt.Fprintf(&b, "state_transitions   : %d\n", r.StateTransitions)
	fmt.Fprintf(&b, "transitions         : %s\n", r.joinStates())
	fmt.Fprintf(&b, "illegal_transitions : %d\n", r.IllegalTransitions)
	fmt.Fprintf(&b, "rtp_received        : %d\n", r.RTPReceived)
	fmt.Fprintf(&b, "rtp_duplicates      : %d\n", r.RTPDuplicates)
	fmt.Fprintf(&b, "rtp_reordered       : %d\n", r.RTPReordered)
	fmt.Fprintf(&b, "seq_gap_segments    : %d\n", r.SeqGapSegments)
	fmt.Fprintf(&b, "seq_gap_packets     : %d\n", r.SeqGapPackets)
	for i, g := range r.Gaps {
		fmt.Fprintf(&b, "  gap[%d]            : seq %d..%d (%d packets)\n", i, g.First, g.Last, g.Count)
	}
	fmt.Fprintf(&b, "max_reorder_depth   : %d\n", r.MaxReorderDepth)
	fmt.Fprintf(&b, "frames_reassembled  : %d\n", r.FramesReassembled)
	fmt.Fprintf(&b, "bytes               : %d (%.2f MiB)\n", r.Bytes, float64(r.Bytes)/(1<<20))
	fmt.Fprintf(&b, "seq_range           : %d..%d\n", r.FirstSeq, r.LastSeq)
	fmt.Fprintf(&b, "reconnects          : %d\n", r.Reconnects)
	if r.Error != "" {
		fmt.Fprintf(&b, "error               : %s (exit %d)\n", r.Error, r.ExitCode)
	}
	return b.String()
}

func (r Report) joinStates() string {
	names := make([]string, 0, len(r.Transitions)+1)
	if len(r.Transitions) > 0 {
		names = append(names, string(r.Transitions[0].From))
	}
	for _, tr := range r.Transitions {
		if tr.Legal {
			names = append(names, string(tr.To))
		}
	}
	return strings.Join(names, ", ")
}

// Mismatch describes one failed expectation or index inconsistency.
type Mismatch struct {
	Field  string `json:"field"`
	Detail string `json:"detail"`
}

// Verify checks the report against scenario expectations and cross-checks
// every frame against the on-disk index.
func Verify(rep *Report, entries []store.IndexEntry, ex scenario.Expect) []Mismatch {
	var mm []Mismatch
	check := func(ok bool, field, detail string) {
		if !ok {
			mm = append(mm, Mismatch{Field: field, Detail: detail})
		}
	}
	check(rep.StateTransitions == ex.StateTransitions, "state_transitions",
		fmt.Sprintf("got %d want %d", rep.StateTransitions, ex.StateTransitions))
	check(rep.RTPReceived >= ex.RTPReceivedMin && rep.RTPReceived <= ex.RTPReceivedMax,
		"rtp_received", fmt.Sprintf("got %d want [%d,%d]", rep.RTPReceived, ex.RTPReceivedMin, ex.RTPReceivedMax))
	check(rep.RTPDuplicates == ex.Duplicates, "rtp_duplicates",
		fmt.Sprintf("got %d want %d", rep.RTPDuplicates, ex.Duplicates))
	check(rep.RTPReordered == ex.Reordered, "rtp_reordered",
		fmt.Sprintf("got %d want %d", rep.RTPReordered, ex.Reordered))
	check(rep.SeqGapSegments == ex.SeqGapSegments, "seq_gap_segments",
		fmt.Sprintf("got %d want %d", rep.SeqGapSegments, ex.SeqGapSegments))
	check(rep.SeqGapPackets == ex.GapPackets, "seq_gap_packets",
		fmt.Sprintf("got %d want %d", rep.SeqGapPackets, ex.GapPackets))
	check(rep.MaxReorderDepth == ex.MaxReorderDepth, "max_reorder_depth",
		fmt.Sprintf("got %d want %d", rep.MaxReorderDepth, ex.MaxReorderDepth))
	check(rep.FramesReassembled >= ex.FramesMin && rep.FramesReassembled <= ex.FramesMax,
		"frames_reassembled", fmt.Sprintf("got %d want [%d,%d]", rep.FramesReassembled, ex.FramesMin, ex.FramesMax))
	check(rep.Bytes >= ex.BytesMin && rep.Bytes <= ex.BytesMax, "bytes",
		fmt.Sprintf("got %d want [%d,%d]", rep.Bytes, ex.BytesMin, ex.BytesMax))
	if ex.FirstSeq != nil {
		check(rep.FirstSeq == *ex.FirstSeq, "first_seq", fmt.Sprintf("got %d want %d", rep.FirstSeq, *ex.FirstSeq))
	}
	if ex.LastSeq != nil {
		check(rep.LastSeq == *ex.LastSeq, "last_seq", fmt.Sprintf("got %d want %d", rep.LastSeq, *ex.LastSeq))
	}
	check(len(entries) == rep.FramesReassembled, "index_frame_count",
		fmt.Sprintf("index has %d entries, report says %d frames", len(entries), rep.FramesReassembled))

	// Cross-check index entries against the report's sequence boundaries and
	// ensure frames are contiguous and non-overlapping.
	var bytes int64
	for i, e := range entries {
		check(e.FrameIndex == i, "index_order", fmt.Sprintf("entry %d has frame_index %d", i, e.FrameIndex))
		bytes += int64(e.Bytes)
		if i > 0 {
			prev := entries[i-1]
			// Frames must be ordered and non-overlapping; gaps caused by lost
			// packets are legitimate and reported separately as seq_gap_*.
			check(uint32(e.FirstSeq) > uint32(prev.LastSeq), "index_seq_order",
				fmt.Sprintf("frame %d first_seq %d overlaps previous last_seq %d",
					i, e.FirstSeq, prev.LastSeq))
			check(e.SHA256 != "" && e.Bytes > 0, "index_frame_integrity",
				fmt.Sprintf("frame %d missing digest or bytes", i))
		}
	}
	check(bytes == rep.Bytes, "index_bytes",
		fmt.Sprintf("index bytes %d != report bytes %d", bytes, rep.Bytes))
	return mm
}
