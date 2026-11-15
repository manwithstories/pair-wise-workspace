package report

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"path/filepath"
	"strconv"
	"text/tabwriter"
)

// Summary is the final line/count object for a batch run.
type Summary struct {
	Total int `json:"total"`
	Pass  int `json:"pass"`
	Fail  int `json:"fail"`
	Error int `json:"error"`
}

type metricFloat float64

func (m metricFloat) MarshalJSON() ([]byte, error) {
	v := float64(m)
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return []byte("null"), nil
	}
	if v == 0 {
		v = 0
	}
	return []byte(strconv.FormatFloat(v, 'f', 2, 64)), nil
}

type measureJSON struct {
	IntegratedLUFS   metricFloat `json:"integrated_lufs"`
	ShortTermMaxLUFS metricFloat `json:"short_term_max_lufs"`
	MomentaryMaxLUFS metricFloat `json:"momentary_max_lufs"`
	TruePeakDBTP     metricFloat `json:"true_peak_dbtp"`
	GatedBlocks      int         `json:"gated_blocks"`
	Verdict          string      `json:"verdict"`
	Reason           string      `json:"reason,omitempty"`
	NaNs             uint64      `json:"nan_samples,omitempty"`
}

type batchJSON struct {
	File             string      `json:"file"`
	IntegratedLUFS   metricFloat `json:"integrated_lufs"`
	ShortTermMaxLUFS metricFloat `json:"short_term_max_lufs"`
	MomentaryMaxLUFS metricFloat `json:"momentary_max_lufs"`
	TruePeakDBTP     metricFloat `json:"true_peak_dbtp"`
	GatedBlocks      int         `json:"gated_blocks"`
	Verdict          string      `json:"verdict"`
	Reason           string      `json:"reason,omitempty"`
	NaNs             uint64      `json:"nan_samples,omitempty"`
	Error            string      `json:"error,omitempty"`
}

type batchJSONDoc struct {
	Results []batchJSON `json:"results"`
	Summary Summary     `json:"summary"`
}

// RenderMeasure writes one measurement as key=value text or one JSON line.
func RenderMeasure(w io.Writer, row Row, asJSON bool) error {
	if asJSON {
		return writeJSONLine(w, measureJSON{
			IntegratedLUFS:   metricFloat(row.Metrics.IntegratedLUFS),
			ShortTermMaxLUFS: metricFloat(row.Metrics.ShortTermMaxLUFS),
			MomentaryMaxLUFS: metricFloat(row.Metrics.MomentaryMaxLUFS),
			TruePeakDBTP:     metricFloat(row.Metrics.TruePeakDBTP),
			GatedBlocks:      row.Metrics.GatedBlocks, Verdict: row.Verdict,
			Reason: row.Reason, NaNs: row.Metrics.NaNs,
		})
	}
	fmt.Fprintf(w, "integrated_lufs=%s\n", humanNumber(row.Metrics.IntegratedLUFS))
	fmt.Fprintf(w, "short_term_max_lufs=%s\n", humanNumber(row.Metrics.ShortTermMaxLUFS))
	fmt.Fprintf(w, "momentary_max_lufs=%s\n", humanNumber(row.Metrics.MomentaryMaxLUFS))
	fmt.Fprintf(w, "true_peak_dbtp=%s\n", humanNumber(row.Metrics.TruePeakDBTP))
	fmt.Fprintf(w, "gated_blocks=%d\n", row.Metrics.GatedBlocks)
	if row.Metrics.NaNs > 0 {
		fmt.Fprintf(w, "nan_samples=%d\n", row.Metrics.NaNs)
	}
	fmt.Fprintf(w, "verdict=%s\n", row.Verdict)
	if row.Reason != "" {
		fmt.Fprintf(w, "reason=%s\n", row.Reason)
	}
	return nil
}

// RenderBatch writes an aligned table or a single JSON document.
func RenderBatch(w io.Writer, rows []Row, summary Summary, asJSON bool) error {
	if asJSON {
		doc := batchJSONDoc{Summary: summary, Results: make([]batchJSON, 0, len(rows))}
		for _, row := range rows {
			doc.Results = append(doc.Results, batchJSON{
				File:             filepath.Base(row.File),
				IntegratedLUFS:   metricFloat(row.Metrics.IntegratedLUFS),
				ShortTermMaxLUFS: metricFloat(row.Metrics.ShortTermMaxLUFS),
				MomentaryMaxLUFS: metricFloat(row.Metrics.MomentaryMaxLUFS),
				TruePeakDBTP:     metricFloat(row.Metrics.TruePeakDBTP),
				GatedBlocks:      row.Metrics.GatedBlocks, Verdict: row.Verdict,
				Reason: row.Reason, NaNs: row.Metrics.NaNs, Error: row.Err,
			})
		}
		return writeJSONLine(w, doc)
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "FILE\tINTEGRATED\tSHORT_TERM\tMOMENTARY\tTRUE_PEAK\tVERDICT")
	for _, row := range rows {
		if row.Err != "" {
			fmt.Fprintf(tw, "%s\t-\t-\t-\t-\tERROR\terror=%s\n", filepath.Base(row.File), row.Err)
			continue
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s", filepath.Base(row.File),
			humanNumber(row.Metrics.IntegratedLUFS), humanNumber(row.Metrics.ShortTermMaxLUFS),
			humanNumber(row.Metrics.MomentaryMaxLUFS), humanNumber(row.Metrics.TruePeakDBTP), row.Verdict)
		if row.Reason != "" {
			fmt.Fprintf(tw, "\treason=%s", row.Reason)
		}
		fmt.Fprintln(tw)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Fprintf(w, "summary total=%d pass=%d fail=%d error=%d\n", summary.Total, summary.Pass, summary.Fail, summary.Error)
	return nil
}

func writeJSONLine(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = w.Write(b)
	return err
}

func humanNumber(v float64) string {
	if math.IsInf(v, -1) {
		return "-inf"
	}
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return "nan"
	}
	if v == 0 {
		v = 0
	}
	return strconv.FormatFloat(v, 'f', 2, 64)
}
