package engine

import (
	"encoding/json"

	"mediaroute/internal/probe"
)

// Summary is the machine-readable result printed by the CLI and compared
// across full and interrupted/resumed runs.
type Summary struct {
	Input struct {
		Format      string `json:"format"`
		InputSHA256 string `json:"input_sha256"`
		SizeBytes   int64  `json:"size_bytes"`
		Tracks      int    `json:"tracks"`
	} `json:"input"`
	Scan struct {
		Samples       int            `json:"samples"`
		ReusedIndex   bool           `json:"reused_index"`
		IndexRejected bool           `json:"index_rejected"`
		Conflicts     []ConflictView `json:"conflicts"`
		Faults        []string       `json:"non_fatal_faults"`
	} `json:"scan"`
	Keyframe struct {
		Count         int   `json:"count"`
		IntervalP50MS int64 `json:"interval_p50_ms"`
		IntervalP90MS int64 `json:"interval_p90_ms"`
		IntervalP95MS int64 `json:"interval_p95_ms"`
		IntervalP99MS int64 `json:"interval_p99_ms"`
		IntervalMaxMS int64 `json:"interval_max_ms"`
	} `json:"keyframe"`
	Damage struct {
		SampleCount        int     `json:"sample_count"`
		OffsetErrorSamples int     `json:"offset_error_samples"`
		Samples            []int64 `json:"samples"`
		Gaps               int     `json:"gaps"`
	} `json:"damage"`
	Resume struct {
		Interrupted     bool  `json:"interrupted"`
		ElapsedMS       int64 `json:"elapsed_ms"`
		ReplayedSamples int   `json:"replayed_samples"`
		Committed       int   `json:"committed"`
		PausedEvents    int   `json:"paused_events"`
	} `json:"resume"`
	Output struct {
		SHA256 string `json:"sha256"`
	} `json:"output"`
}

// ConflictView is the JSON projection of a probe.ConflictRecord.
type ConflictView struct {
	TrackID uint32 `json:"track_id"`
	Kind    string `json:"kind"`
	Detail  string `json:"detail"`
	Adopted string `json:"adopted"`
}

// BuildSummary assembles the summary from scan and stream results.
// offsetErrorSamples measures how far the first damaged sample sits from
// the gap's projected position; correct packet->sample mapping keeps this
// at zero for healthy runs and <= 1 for damaged runs.
func BuildSummary(sr *ScanResult, st *StreamStats, expectedFirstDamaged int64) *Summary {
	sum := &Summary{}
	rep := sr.Report
	sum.Input.Format = rep.Format
	sum.Input.InputSHA256 = rep.InputSHA256
	sum.Input.SizeBytes = rep.SizeBytes
	sum.Input.Tracks = len(rep.Tracks)
	sum.Scan.Samples = rep.TotalSamples()
	sum.Scan.ReusedIndex = sr.Reused
	sum.Scan.IndexRejected = sr.IndexErr != nil
	for _, c := range rep.Conflicts {
		sum.Scan.Conflicts = append(sum.Scan.Conflicts, ConflictView{
			TrackID: c.TrackID, Kind: c.Kind, Detail: c.Detail, Adopted: c.Adopted,
		})
	}
	for _, fe := range rep.Faults {
		sum.Scan.Faults = append(sum.Scan.Faults, fe.Error())
	}
	if len(rep.Tracks) > 0 {
		tk := rep.Tracks[0]
		ks := rep.KeyStats[tk.TrackID]
		sum.Keyframe.Count = ks.KeyframeCount
		sum.Keyframe.IntervalP50MS = ks.P50MS
		sum.Keyframe.IntervalP90MS = ks.P90MS
		sum.Keyframe.IntervalP95MS = ks.P95MS
		sum.Keyframe.IntervalP99MS = ks.P99MS
		sum.Keyframe.IntervalMaxMS = ks.MaxMS
	}
	sum.Damage.SampleCount = len(st.DamagedSamples)
	sum.Damage.Samples = st.DamagedSamples
	sum.Damage.Gaps = len(st.Gaps)
	sum.Damage.OffsetErrorSamples = offsetError(st.DamagedSamples, expectedFirstDamaged)
	sum.Resume.Interrupted = st.Interrupted
	sum.Resume.ElapsedMS = (st.ElapsedNanos + 500_000) / 1_000_000
	sum.Resume.ReplayedSamples = st.ReplayedSamples
	sum.Resume.Committed = st.Committed
	sum.Resume.PausedEvents = st.PausedEvents
	sum.Output.SHA256 = st.OutputSHA256
	return sum
}

// offsetError returns 0 for a healthy run; for a damaged run it is the
// absolute distance between the first located damaged sample and the
// packet-gap projection, capped at one sample.
func offsetError(damaged []int64, expectedFirst int64) int {
	if len(damaged) == 0 {
		return 0
	}
	if expectedFirst < 0 {
		return 0
	}
	diff := damaged[0] - expectedFirst
	if diff < 0 {
		diff = -diff
	}
	if diff > 1 {
		return int(diff)
	}
	return int(diff)
}

// JSON renders the summary as indented JSON for terminal output and for
// the full-vs-resumed equality check.
func (s *Summary) JSON() ([]byte, error) {
	return json.MarshalIndent(s, "", "  ")
}

// Equal reports whether two summaries are identical, comparing the JSON
// bytes so nested field drift is caught wholesale.
func (s *Summary) Equal(other *Summary) bool {
	a, _ := s.JSON()
	b, _ := other.JSON()
	return string(a) == string(b)
}

var _ = probe.ScannerVersion
