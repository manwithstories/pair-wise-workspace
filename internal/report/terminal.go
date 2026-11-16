// Package report renders measurement results as a terminal table and
// as a JSON summary. Floating-point values are emitted with exactly
// three decimals; undecidable values are JSON null.
package report

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"text/tabwriter"

	"loudcheck/internal/meter"
)

// Fixed3 marshals a float with exactly three decimal places.
type Fixed3 float64

// MarshalJSON implements json.Marshaler.
func (f Fixed3) MarshalJSON() ([]byte, error) {
	return []byte(strconv.FormatFloat(float64(f), 'f', 3, 64)), nil
}

// FileReport is the JSON view of one measured file.
type FileReport struct {
	File           string   `json:"file"`
	Layout         string   `json:"layout"`
	LFEIncluded    bool     `json:"lfe_included"`
	IntegratedLUFS *Fixed3  `json:"integrated_lufs"`
	ShortLUFSMax   *Fixed3  `json:"short_lufs_max"`
	TruePeakDBTP   *Fixed3  `json:"true_peak_dbtp"`
	Samples        int64    `json:"samples"`
	Windows        int64    `json:"windows"`
	GatedWindows   int64    `json:"gated_windows"`
	DurationMs     int64    `json:"duration_ms"`
	LoudnessPass   bool     `json:"loudness_pass"`
	TruePeakPass   bool     `json:"true_peak_pass"`
	Decision       string   `json:"decision"`
	Reason         string   `json:"reason"`
	Error          *string  `json:"error"`
}

// Summary is the batch aggregate.
type Summary struct {
	Files  int `json:"files"`
	Passed int `json:"passed"`
	Failed int `json:"failed"`
	Errors int `json:"errors"`
}

// Document is the top-level JSON report.
type Document struct {
	Summary Summary      `json:"summary"`
	Results []FileReport `json:"results"`
}

// FileError pairs a file with a processing error.
type FileError struct {
	File string
	Err  error
}

// Build assembles the report document from results and errors.
func Build(results []*meter.Result, errs []FileError) Document {
	doc := Document{Results: make([]FileReport, 0, len(results)+len(errs))}
	for _, r := range results {
		doc.Results = append(doc.Results, convert(r, nil))
		doc.Summary.Files++
		switch r.Decision {
		case meter.DecisionPass:
			doc.Summary.Passed++
		default:
			doc.Summary.Failed++
		}
	}
	for _, fe := range errs {
		msg := fe.Err.Error()
		doc.Results = append(doc.Results, convert(&meter.Result{File: fe.File}, &msg))
		doc.Summary.Files++
		doc.Summary.Errors++
	}
	return doc
}

func convert(r *meter.Result, errMsg *string) FileReport {
	fr := FileReport{
		File:         r.File,
		Layout:       r.Layout,
		LFEIncluded:  r.LFEIncluded,
		Samples:      r.Samples,
		Windows:      r.Windows,
		GatedWindows: r.GatedWindows,
		DurationMs:   r.Duration.Milliseconds(),
		LoudnessPass: r.LoudnessPass,
		TruePeakPass: r.TruePeakPass,
		Decision:     r.Decision,
		Reason:       r.Reason,
		Error:        errMsg,
	}
	if errMsg != nil {
		fr.Decision = "error"
		fr.Reason = "processing error"
	}
	if r.IntegratedLUFS != nil {
		v := Fixed3(*r.IntegratedLUFS)
		fr.IntegratedLUFS = &v
	}
	if r.ShortLUFSMax != nil {
		v := Fixed3(*r.ShortLUFSMax)
		fr.ShortLUFSMax = &v
	}
	if r.TruePeakDBTP != nil {
		v := Fixed3(*r.TruePeakDBTP)
		fr.TruePeakDBTP = &v
	}
	return fr
}

// WriteJSON writes the document as indented JSON.
func WriteJSON(w io.Writer, doc Document) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(doc)
}

// WriteTable writes a human-readable table plus the summary line.
func WriteTable(w io.Writer, doc Document) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "FILE\tINTEGRATED\tSHORT_MAX\tTRUE_PEAK\tDECISION\tREASON")
	for _, r := range doc.Results {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			r.File, ptr(r.IntegratedLUFS), ptr(r.ShortLUFSMax), ptr(r.TruePeakDBTP),
			r.Decision, r.Reason)
	}
	tw.Flush()
	s := doc.Summary
	fmt.Fprintf(w, "files=%d passed=%d failed=%d errors=%d\n",
		s.Files, s.Passed, s.Failed, s.Errors)
}

func ptr(f *Fixed3) string {
	if f == nil {
		return "-"
	}
	return strconv.FormatFloat(float64(*f), 'f', 3, 64)
}
