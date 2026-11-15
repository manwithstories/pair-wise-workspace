// Package report aggregates numeric results and findings from the
// concurrent expand/verify pipeline and prints them deterministically.
package report

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"dashkit/internal/model"
)

// Finding is a report-level view of a model problem.
type Finding struct {
	Code string `json:"code"`
	Rep  string `json:"rep,omitempty"`
	Msg  string `json:"msg"`
}

// Report collects counters, URLs and findings. All mutators are locked so
// concurrent per-representation workers can share one instance.
type Report struct {
	mu                     sync.Mutex
	Representations        int       `json:"representations"`
	Video                  int       `json:"video"`
	Audio                  int       `json:"audio"`
	Periods                int       `json:"periods"`
	SegmentsTotal          int       `json:"segments_total"`
	ExpandedURLs           int       `json:"expanded_urls"`
	MaxTimelineDeviationMS string    `json:"max_timeline_deviation_ms"`
	TickRemainder          int64     `json:"tick_remainder"`
	PTODriftTicks          string    `json:"pto_drift_ticks"`
	AlignmentMismatches    int       `json:"alignment_mismatches"`
	FirstURLs              []string  `json:"first_urls,omitempty"`
	LastURLs               []string  `json:"last_urls,omitempty"`
	Findings               []Finding `json:"findings,omitempty"`
}

// New returns an empty report with zero-valued rational strings.
func New() *Report {
	return &Report{MaxTimelineDeviationMS: "0", PTODriftTicks: "0"}
}

// AddSegments records one representation's expanded output.
func (r *Report) AddSegments(segCount, urlCount int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.SegmentsTotal += segCount
	r.ExpandedURLs += urlCount
}

// AddFinding appends a structured finding, dropping exact duplicates.
func (r *Report) AddFinding(p model.Problem) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, f := range r.Findings {
		if f.Code == p.Code && f.Rep == p.Rep && f.Msg == p.Msg {
			return
		}
	}
	r.Findings = append(r.Findings, Finding{Code: p.Code, Rep: p.Rep, Msg: p.Msg})
}

// SetURLs records the first/last 3 expanded URLs for the demo printout.
func (r *Report) SetURLs(all []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := len(all)
	head := 3
	if n < head {
		head = n
	}
	r.FirstURLs = append([]string(nil), all[:head]...)
	if n > 3 {
		r.LastURLs = append([]string(nil), all[n-3:]...)
	}
}

// SortedFindings returns findings ordered by code then rep for stable
// scripting output.
func (r *Report) SortedFindings() []Finding {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sortedFindingsLocked()
}

func (r *Report) sortedFindingsLocked() []Finding {
	out := append([]Finding(nil), r.Findings...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Code != out[j].Code {
			return out[i].Code < out[j].Code
		}
		return out[i].Rep < out[j].Rep
	})
	return out
}

// Print writes key=value lines plus URLs and findings to w.
func (r *Report) Print(w io.Writer) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fmt.Fprintf(w, "representations=%d\n", r.Representations)
	fmt.Fprintf(w, "video=%d\n", r.Video)
	fmt.Fprintf(w, "audio=%d\n", r.Audio)
	fmt.Fprintf(w, "periods=%d\n", r.Periods)
	fmt.Fprintf(w, "segments_total=%d\n", r.SegmentsTotal)
	fmt.Fprintf(w, "expanded_urls=%d\n", r.ExpandedURLs)
	fmt.Fprintf(w, "max_timeline_deviation_ms=%s\n", r.MaxTimelineDeviationMS)
	fmt.Fprintf(w, "tick_remainder=%d\n", r.TickRemainder)
	fmt.Fprintf(w, "pto_drift_ticks=%s\n", r.PTODriftTicks)
	fmt.Fprintf(w, "alignment_mismatches=%d\n", r.AlignmentMismatches)
	for _, u := range r.FirstURLs {
		fmt.Fprintf(w, "first_url=%s\n", u)
	}
	for _, u := range r.LastURLs {
		fmt.Fprintf(w, "last_url=%s\n", u)
	}
	for _, f := range r.sortedFindingsLocked() {
		if f.Rep != "" {
			fmt.Fprintf(w, "finding=%s rep=%s %s\n", f.Code, f.Rep, f.Msg)
		} else {
			fmt.Fprintf(w, "finding=%s %s\n", f.Code, f.Msg)
		}
	}
}

// WriteJSON stores the report next to stdout output.
func (r *Report) WriteJSON(path string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return os.WriteFile(path, data, 0o644)
}
