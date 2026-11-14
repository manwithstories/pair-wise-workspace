package verify

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
)

// Failure is one stable, machine-readable verification finding.
type Failure struct {
	Code        string `json:"code"`
	Variant     string `json:"variant"`
	Sequence    int64  `json:"sequence"`
	EndSequence int64  `json:"end_sequence,omitempty"`
	URI         string `json:"uri,omitempty"`
	Message     string `json:"message"`
	Expected    string `json:"expected,omitempty"`
	Actual      string `json:"actual,omitempty"`
}

// VariantReport contains the requested per-rendition metrics.
type VariantReport struct {
	Name                  string    `json:"name"`
	Playlist              string    `json:"playlist"`
	BitrateAdvertisedKbps int64     `json:"bitrate_advertised_kbps"`
	BitrateMeasuredKbps   int64     `json:"bitrate_measured_kbps"`
	SegmentsExpected      int       `json:"segments_expected"`
	SegmentsFound         int       `json:"segments_found"`
	DurationDevMaxMS      int64     `json:"duration_dev_max_ms"`
	DurationDevMeanMS     int64     `json:"duration_dev_mean_ms"`
	FirstErrorSequence    *int64    `json:"first_error_sequence,omitempty"`
	Failures              []Failure `json:"failures"`
}

// Report is the top-level deterministic verification result.
type Report struct {
	Root               string          `json:"root"`
	Variants           []VariantReport `json:"variants"`
	FirstErrorSequence *int64          `json:"first_error_sequence,omitempty"`
	Failures           []Failure       `json:"failures"`
}

// Failed reports whether any finding was recorded.
func (r *Report) Failed() bool { return len(r.Failures) > 0 }

// Finalize sorts findings and fills aggregate failure lists.
func (r *Report) Finalize() {
	variantOrder := make(map[string]int, len(r.Variants))
	for i, variant := range r.Variants {
		variantOrder[variant.Name] = i
	}
	for i := range r.Variants {
		variant := &r.Variants[i]
		if variant.Failures == nil {
			variant.Failures = []Failure{}
		}
		sortFailures(variant.Failures, variantOrder)
		if len(variant.Failures) > 0 {
			seq := variant.Failures[0].Sequence
			variant.FirstErrorSequence = &seq
		}
	}
	r.Failures = r.Failures[:0]
	for _, variant := range r.Variants {
		r.Failures = append(r.Failures, variant.Failures...)
	}
	if r.Failures == nil {
		r.Failures = []Failure{}
	}
	sortFailures(r.Failures, variantOrder)
	if len(r.Failures) > 0 {
		seq := r.Failures[0].Sequence
		r.FirstErrorSequence = &seq
	}
}

func sortFailures(failures []Failure, order map[string]int) {
	sort.SliceStable(failures, func(i, j int) bool {
		a, b := failures[i], failures[j]
		if a.Sequence != b.Sequence {
			return a.Sequence < b.Sequence
		}
		if order[a.Variant] != order[b.Variant] {
			return order[a.Variant] < order[b.Variant]
		}
		if a.Code != b.Code {
			return a.Code < b.Code
		}
		return a.URI < b.URI
	})
}

// WriteJSON writes indented JSON with a trailing newline.
func (r *Report) WriteJSON(w io.Writer) error {
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	return encoder.Encode(r)
}

// WriteTable renders one summary row per variant plus compact failure detail.
func (r *Report) WriteTable(w io.Writer) error {
	if _, err := fmt.Fprintln(w, "VARIANT  ADVERTISED  MEASURED  EXPECTED  FOUND  MAX_DEV_MS  MEAN_DEV_MS  FAILURES"); err != nil {
		return err
	}
	for _, variant := range r.Variants {
		if _, err := fmt.Fprintf(w, "%-8s %11d %9d %9d %6d %11d %12d %9d\n",
			variant.Name, variant.BitrateAdvertisedKbps, variant.BitrateMeasuredKbps,
			variant.SegmentsExpected, variant.SegmentsFound,
			variant.DurationDevMaxMS, variant.DurationDevMeanMS, len(variant.Failures)); err != nil {
			return err
		}
	}
	if len(r.Failures) == 0 {
		return nil
	}
	if _, err := fmt.Fprintln(w, "\nFAILURES"); err != nil {
		return err
	}
	for _, failure := range r.Failures {
		if _, err := fmt.Fprintf(w, "%-8s %-18s seq=%-6d uri=%s  %s\n",
			failure.Variant, failure.Code, failure.Sequence, failure.URI, failure.Message); err != nil {
			return err
		}
	}
	return nil
}
