package pipeline

import (
	"fmt"
	"strings"
)

// Config controls one demo/verification run.
type Config struct {
	Case    string
	Seed    int64
	Strict  bool
	JSON    bool
	WorkDir string
}

// SegmentHash is one deterministic per-segment manifest entry.
type SegmentHash struct {
	Segment       int    `json:"segment"`
	SegmentSHA256 string `json:"segment_sha256"`
}

// Report is the terminal and JSON result.
type Report struct {
	Case               string        `json:"case"`
	Seed               int64         `json:"seed"`
	RoundtripPercent   float64       `json:"roundtrip_percent"`
	CuesOK             int           `json:"cues_ok"`
	CuesTotal          int           `json:"cues_total"`
	KeyframesFound     int           `json:"keyframes_found"`
	KeyframesExpected  int           `json:"keyframes_expected"`
	PacketsReceived    int           `json:"packets_received"`
	Duplicates         int           `json:"duplicates"`
	Reordered          int           `json:"reordered"`
	Delivered          int           `json:"delivered"`
	BurstLost          int           `json:"burst_lost"`
	BadCRC             int           `json:"bad_crc"`
	BandwidthDrops     int           `json:"bandwidth_drops"`
	InvalidDF          int           `json:"invalid_df"`
	MidnightWrap       bool          `json:"midnight_wrap"`
	TimecodeBase       string        `json:"timecode_base"`
	TimecodeAligned    bool          `json:"timecode_aligned"`
	IsolatedIssues     int           `json:"isolated_issues"`
	Recovered          int           `json:"recovered"`
	DamagedSegments    int           `json:"damaged_segments"`
	Monotonic          bool          `json:"monotonic"`
	ConflictCount      int           `json:"conflict_count"`
	CuttableBoundaries int           `json:"cuttable_boundaries"`
	HashOK             bool          `json:"hash_ok"`
	IndexResumed       bool          `json:"index_resumed"`
	Strict             bool          `json:"strict"`
	SegmentSHA256      string        `json:"segment_sha256"`
	SegmentHashes      []SegmentHash `json:"segment_hashes"`
}

// IOError marks a local filesystem read/write failure for exit code 4.
type IOError struct {
	Op  string
	Err error
}

func (e *IOError) Error() string { return fmt.Sprintf("local I/O failure during %s: %v", e.Op, e.Err) }
func (e *IOError) Unwrap() error { return e.Err }

// ExitCode maps report state to the specified process status.
func (r *Report) ExitCode() int {
	if r.Strict && r.DamagedSegments > 0 {
		return 3
	}
	return 0
}

// String renders the stable key=value terminal report.
func (r *Report) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "medcheck report case=%s seed=%d\n", r.Case, r.Seed)
	fmt.Fprintf(&b, "roundtrip=%.2f%%\n", r.RoundtripPercent)
	fmt.Fprintf(&b, "keyframe_hit=%d/%d\n", r.KeyframesFound, r.KeyframesExpected)
	fmt.Fprintf(&b, "cues=%d/%d\n", r.CuesOK, r.CuesTotal)
	fmt.Fprintf(&b, "packets=%d duplicates=%d reordered=%d burst_lost=%d bad_crc=%d\n",
		r.PacketsReceived, r.Duplicates, r.Reordered, r.BurstLost, r.BadCRC)
	fmt.Fprintf(&b, "bandwidth_drop=%d invalid_df=%d midnight_wrap=%t timecode=%s subtitle_tc_aligned=%t isolated_issues=%d\n",
		r.BandwidthDrops, r.InvalidDF, r.MidnightWrap, r.TimecodeBase, r.TimecodeAligned, r.IsolatedIssues)
	fmt.Fprintf(&b, "recovered=%d damaged_segments=%d monotonic=%t\n",
		r.Recovered, r.DamagedSegments, r.Monotonic)
	fmt.Fprintf(&b, "boundary conflicts=%d cuttable=%d hash_ok=%t\n",
		r.ConflictCount, r.CuttableBoundaries, r.HashOK)
	fmt.Fprintf(&b, "index resumed=%t strict=%t\n", r.IndexResumed, r.Strict)
	fmt.Fprintf(&b, "segment_sha256=%s\n", r.SegmentSHA256)
	for _, h := range r.SegmentHashes {
		fmt.Fprintf(&b, "segment_hash segment=%02d sha256=%s\n", h.Segment, h.SegmentSHA256)
	}
	if code := r.ExitCode(); code == 3 {
		fmt.Fprintln(&b, "verdict=strict-mode-bad-segment")
	} else {
		fmt.Fprintln(&b, "verdict=report-complete")
	}
	return b.String()
}
