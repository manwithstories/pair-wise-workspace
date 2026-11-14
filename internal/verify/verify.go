package verify

import (
	"fmt"
	"strings"

	"subtool/internal/subtitle"
	"subtool/internal/timebase"
)

type Drop struct {
	Index   int    `json:"index"`
	Reason  string `json:"reason"`
	StartMS int64  `json:"in_ms"`
	EndMS   int64  `json:"out_ms"`
	Text    string `json:"text,omitempty"`
}

type CueDelta struct {
	Index        int    `json:"index"`
	Position     int    `json:"position"`
	InMS         int64  `json:"in_ms"`
	OutMS        int64  `json:"out_ms"`
	InFrame      int64  `json:"in_frame"`
	OutFrame     int64  `json:"out_frame"`
	ActualInMS   int64  `json:"actual_in_ms"`
	ActualOutMS  int64  `json:"actual_out_ms"`
	StartDriftMS int64  `json:"start_drift_ms"`
	EndDriftMS   int64  `json:"end_drift_ms"`
	DriftMS      int64  `json:"drift_ms"`
	TextMatch    bool   `json:"text_match"`
	Reason       string `json:"reason,omitempty"`
}

type Result struct {
	CueCountMatch  bool       `json:"cue_count_match"`
	MaxAbsDriftMS  int64      `json:"max_abs_drift_ms"`
	MeanAbsDriftMS float64    `json:"mean_abs_drift_ms"`
	Dropped        []Drop     `json:"dropped"`
	DroppedTotal   int        `json:"dropped_total"`
	MissingCues    int        `json:"missing_cues"`
	ExtraCues      int        `json:"extra_cues"`
	TextMismatches int        `json:"text_mismatches"`
	Deltas         []CueDelta `json:"deltas"`
}

func Compare(expected, actual *subtitle.Track, inRate, outRate timebase.Rate) Result {
	result := Result{Dropped: []Drop{}, Deltas: []CueDelta{}}
	if expected == nil {
		result.MissingCues = 1
		return result
	}
	if actual == nil {
		actual = &subtitle.Track{}
	}
	for _, cue := range expected.Cues {
		if cue.Invalid || cue.Reason != "" {
			reason := cue.Reason
			if reason == "" {
				reason = "invalid_cue"
			}
			result.Dropped = append(result.Dropped, Drop{Index: cue.Index, Reason: reason,
				StartMS: cue.StartMS, EndMS: cue.EndMS, Text: cue.Text})
		}
	}
	eligible := subtitle.Eligible(expected.Cues)
	result.CueCountMatch = len(eligible) == len(actual.Cues)
	limit := len(eligible)
	if len(actual.Cues) < limit {
		limit = len(actual.Cues)
		result.MissingCues = len(eligible) - len(actual.Cues)
	}
	if len(actual.Cues) > len(eligible) {
		result.ExtraCues = len(actual.Cues) - len(eligible)
	}
	var driftSum int64
	var driftCount int64
	for i := 0; i < limit; i++ {
		want, got := eligible[i], actual.Cues[i]
		startDrift := abs64(got.StartMS - want.StartMS)
		endDrift := abs64(got.EndMS - want.EndMS)
		drift := max64(startDrift, endDrift)
		delta := CueDelta{
			Index: want.Index, Position: i + 1, InMS: want.StartMS, OutMS: want.EndMS,
			InFrame:    timebase.FrameForMS(want.StartMS, inRate),
			OutFrame:   timebase.FrameForMS(want.EndMS, outRate),
			ActualInMS: got.StartMS, ActualOutMS: got.EndMS,
			StartDriftMS: startDrift, EndDriftMS: endDrift, DriftMS: drift,
			TextMatch: normalize(want.Text) == normalize(got.Text),
		}
		if !delta.TextMatch {
			delta.Reason = "text_mismatch"
			result.TextMismatches++
		}
		if drift > result.MaxAbsDriftMS {
			result.MaxAbsDriftMS = drift
		}
		driftSum += startDrift + endDrift
		driftCount += 2
		result.Deltas = append(result.Deltas, delta)
	}
	if driftCount > 0 {
		result.MeanAbsDriftMS = float64(driftSum) / float64(driftCount)
	}
	result.DroppedTotal = len(result.Dropped)
	return result
}

func (r Result) Passed(toleranceMS int64, via subtitle.Format) bool {
	driftLimit := toleranceMS
	if via == subtitle.FormatASS && driftLimit < 5 {
		driftLimit = 5
	}
	return r.MaxAbsDriftMS <= driftLimit && r.CueCountMatch && r.TextMismatches == 0 &&
		r.MissingCues == 0 && r.ExtraCues == 0
}

func (r Result) QuantizedMaxMS(via subtitle.Format) int64 {
	if via == subtitle.FormatASS {
		return r.MaxAbsDriftMS
	}
	return 0
}

func (r Result) Summary() string {
	return fmt.Sprintf("max_drift=%dms mean_drift=%.3fms dropped=%d count_match=%t",
		r.MaxAbsDriftMS, r.MeanAbsDriftMS, r.DroppedTotal, r.CueCountMatch)
}

func normalize(text string) string {
	return strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
}

func abs64(value int64) int64 {
	if value < 0 {
		return -value
	}
	return value
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
