package demo

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"text/tabwriter"
)

// Print writes the demo result. With jsonOut it emits a single JSON document
// including every acceptance metric, the fault-variant checks and the artifact
// digests; otherwise it writes a compact terminal table.
func Print(w io.Writer, res *Result, jsonOut bool) {
	PrintMode(w, res, jsonOut, true)
}

// PrintMode renders a result; when streamed is false the table omits the
// streaming metrics so the scan command does not report misleading zeros.
func PrintMode(w io.Writer, res *Result, jsonOut, streamed bool) {
	if res == nil {
		return
	}
	if jsonOut {
		enc := json.NewEncoder(w)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		_ = enc.Encode(BuildJSON(res))
		return
	}
	printTable(w, res, streamed)
}

// JSONReport is the machine-readable shape of the demo. The head-line fields
// are exactly the documented acceptance metrics; details follow.
type JSONReport struct {
	SyncErrorMSP95     float64 `json:"sync_error_ms_p95"`
	PacketRecoveryRate float64 `json:"packet_recovery_rate"`
	LatencyMSP95       float64 `json:"latency_ms_p95"`
	IndexedSamples     int     `json:"indexed_samples"`
	CorruptRanges      int     `json:"corrupt_ranges"`
	IndexRebuilt       bool    `json:"index_rebuilt"`
	CacheHit           bool    `json:"cache_hit"`
	Stable             bool    `json:"artifact_stable"`
	ExitCode           int     `json:"exit_code"`

	SyncErrorMSMax    float64           `json:"sync_error_ms_max"`
	SyncAbnormalCount int               `json:"sync_abnormal_count"`
	PacketsReceived   int               `json:"packets_received"`
	PacketsRecovered  int               `json:"packets_recovered"`
	PacketsLost       int               `json:"packets_lost"`
	PacketsDuplicate  int               `json:"packets_duplicate"`
	LatencyMSMax      float64           `json:"latency_ms_max"`
	ProcessedSamples  int               `json:"processed_samples"`
	IsolatedSamples   int               `json:"isolated_samples"`
	AudioBlocks       int               `json:"audio_blocks"`
	AudioFrames       int64             `json:"audio_frames"`
	OutputLUFS        float64           `json:"output_lufs"`
	TruePeakDBTP      float64           `json:"true_peak_dbtp"`
	GainDB            float64           `json:"gain_db"`
	LoudnessLimited   bool              `json:"loudness_limited"`
	LayoutResolved    string            `json:"layout_resolved"`
	LayoutFallback    bool              `json:"layout_fallback"`
	LayoutReason      string            `json:"layout_reason"`
	PeakMemoryBytes   int               `json:"peak_memory_bytes"`
	IndexSummary      string            `json:"index_summary"`
	ArtifactSummary   string            `json:"artifact_summary"`
	ArtifactDigests   []string          `json:"artifact_digests"`
	ElapsedMS         float64           `json:"elapsed_ms"`
	CorruptSpans      []CorruptSpanJSON `json:"corrupt_spans,omitempty"`
	SyncSamples       []SyncSampleJSON  `json:"sync_samples,omitempty"`
	Faults            []FaultJSON       `json:"faults,omitempty"`
	Warnings          []string          `json:"warnings,omitempty"`
}

// CorruptSpanJSON is a reported isolated interval in the JSON report.
type CorruptSpanJSON struct {
	TrackID     uint32  `json:"track_id"`
	FirstSample int64   `json:"first_sample"`
	LastSample  int64   `json:"last_sample"`
	ByteOffset  int64   `json:"byte_offset"`
	Reason      string  `json:"reason"`
	StartMS     float64 `json:"start_ms"`
	EndMS       float64 `json:"end_ms"`
}

// SyncSampleJSON exposes the source, edit-mapped and output views.
type SyncSampleJSON struct {
	TrackID   uint32  `json:"track_id"`
	Sample    int64   `json:"sample"`
	SourceMS  float64 `json:"source_ms"`
	EditMS    float64 `json:"edit_mapped_ms"`
	OutputMS  float64 `json:"output_ms"`
	Deviation float64 `json:"deviation_ms"`
	Abnormal  bool    `json:"abnormal"`
}

// FaultJSON is one corruption-variant verification.
type FaultJSON struct {
	Variant          string `json:"variant"`
	Recoverable      bool   `json:"recoverable"`
	CorruptRanges    int    `json:"corrupt_ranges"`
	IndexedSamples   int    `json:"indexed_samples"`
	IsolatedSamples  int    `json:"isolated_samples"`
	StreamBoundaryOK bool   `json:"stream_boundary_ok"`
	Detail           string `json:"detail,omitempty"`
}

// BuildJSON projects a Result onto the documented JSON shape.
func BuildJSON(res *Result) JSONReport {
	out := JSONReport{Stable: res.Stable, ExitCode: res.ExitCode}
	if len(res.Takes) > 0 {
		rep := res.Takes[len(res.Takes)-1].Report
		out.SyncErrorMSP95 = round(rep.SyncErrorMSP95, 3)
		out.PacketRecoveryRate = round(rep.PacketRecoveryRate, 4)
		out.LatencyMSP95 = round(rep.LatencyMSP95, 3)
		out.IndexedSamples = rep.IndexedSamples
		out.CorruptRanges = rep.CorruptRanges
		out.IndexRebuilt = rep.IndexRebuilt
		out.CacheHit = rep.CacheHit
		out.SyncErrorMSMax = round(rep.SyncErrorMSMax, 3)
		out.SyncAbnormalCount = rep.SyncAbnormalCount
		out.PacketsReceived = rep.PacketsReceived
		out.PacketsRecovered = rep.PacketsRecovered
		out.PacketsLost = rep.PacketsLost
		out.PacketsDuplicate = rep.PacketsDuplicate
		out.LatencyMSMax = round(rep.LatencyMSMax, 3)
		out.ProcessedSamples = rep.ProcessedSamples
		out.IsolatedSamples = rep.IsolatedSamples
		out.AudioBlocks = rep.AudioBlocks
		out.AudioFrames = rep.AudioFrames
		out.OutputLUFS = round(rep.OutputLUFS, 2)
		out.TruePeakDBTP = round(rep.TruePeakDBTP, 2)
		out.GainDB = round(rep.GainDB, 2)
		out.LoudnessLimited = rep.LoudnessLimited
		out.LayoutResolved = rep.LayoutResolved
		out.LayoutFallback = rep.LayoutFallback
		out.LayoutReason = rep.LayoutReason
		out.PeakMemoryBytes = rep.PeakMemoryBytes
		out.IndexSummary = rep.IndexSummary
		out.ArtifactSummary = rep.ArtifactSummary
		out.ElapsedMS = round(rep.ElapsedMS, 3)
		for _, s := range rep.CorruptSpans {
			out.CorruptSpans = append(out.CorruptSpans, CorruptSpanJSON{
				TrackID: s.TrackID, FirstSample: s.FirstSample, LastSample: s.LastSample,
				ByteOffset: s.ByteOffset, Reason: s.Reason,
				StartMS: round(s.StartMS, 3), EndMS: round(s.EndMS, 3),
			})
		}
		for _, s := range rep.SyncSamples {
			out.SyncSamples = append(out.SyncSamples, SyncSampleJSON{
				TrackID: s.TrackID, Sample: s.Sample,
				SourceMS:  round(s.Source.Milliseconds(), 3),
				EditMS:    round(s.EditMapped.Milliseconds(), 3),
				OutputMS:  round(s.Output.Milliseconds(), 3),
				Deviation: round(s.Deviation, 3), Abnormal: s.Abnormal,
			})
		}
		out.Warnings = append(out.Warnings, rep.Warnings...)
	}
	for _, t := range res.Takes {
		out.ArtifactDigests = append(out.ArtifactDigests, t.Artifact)
	}
	for _, f := range res.Faults {
		out.Faults = append(out.Faults, FaultJSON{
			Variant: string(f.Variant), Recoverable: f.Recoverable,
			CorruptRanges: f.CorruptRanges, IndexedSamples: f.IndexedSamples,
			IsolatedSamples: f.IsolatedSamples, StreamBoundaryOK: f.StreamBoundaryOK, Detail: f.Detail,
		})
	}
	return out
}

func printTable(w io.Writer, res *Result, streamed bool) {
	if len(res.Takes) == 0 {
		fmt.Fprintln(w, "no runs")
		return
	}
	rep := res.Takes[len(res.Takes)-1].Report
	tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
	fmt.Fprintf(tw, "METRIC\tVALUE\tTARGET\n")
	if streamed {
		fmt.Fprintf(tw, "sync_error_ms_p95\t%.2f\t<= 20\n", rep.SyncErrorMSP95)
		fmt.Fprintf(tw, "packet_recovery_rate\t%.3f\t>= 0.95\n", rep.PacketRecoveryRate)
		fmt.Fprintf(tw, "latency_ms_p95\t%.2f\t<= 250\n", rep.LatencyMSP95)
	}
	fmt.Fprintf(tw, "indexed_samples\t%d\t-\n", rep.IndexedSamples)
	fmt.Fprintf(tw, "corrupt_ranges\t%d\t-\n", rep.CorruptRanges)
	fmt.Fprintf(tw, "index_rebuilt\t%v\t-\n", rep.IndexRebuilt)
	fmt.Fprintf(tw, "cache_hit\t%v\t-\n", rep.CacheHit)
	if streamed {
		fmt.Fprintf(tw, "output_lufs\t%.2f\t-16\n", rep.OutputLUFS)
		fmt.Fprintf(tw, "true_peak_dbtp\t%.2f\t<= -1\n", rep.TruePeakDBTP)
		fmt.Fprintf(tw, "layout\t%s (fallback=%v)\t48k stereo\n", rep.LayoutResolved, rep.LayoutFallback)
	}
	fmt.Fprintf(tw, "artifact_stable\t%v\t-\n", res.Stable)
	fmt.Fprintf(tw, "artifact_summary\t%s\t-\n", shortDigest(rep.ArtifactSummary))
	fmt.Fprintf(tw, "exit_code\t%d\t0\n", res.ExitCode)
	tw.Flush()
	if len(res.Takes) > 1 {
		fmt.Fprintln(w, "\nruns:")
		for i, t := range res.Takes {
			fmt.Fprintf(w, "  run %d: hit=%v rebuilt=%v artifact=%s\n", i+1, t.Report.CacheHit, t.Report.IndexRebuilt, shortDigest(t.Artifact))
		}
	}
	if len(res.Faults) > 0 {
		fmt.Fprintln(w, "\nfault variants:")
		for _, f := range res.Faults {
			fmt.Fprintf(w, "  %-14s recoverable=%v ranges=%d indexed=%d isolated=%d boundary_ok=%v\n",
				f.Variant, f.Recoverable, f.CorruptRanges, f.IndexedSamples, f.IsolatedSamples, f.StreamBoundaryOK)
		}
	}
	if streamed {
		if err := metricChecks(res); len(err) > 0 {
			fmt.Fprintln(w, "\nthreshold failures:")
			for _, e := range err {
				fmt.Fprintln(w, "  -", e)
			}
		}
	}
}

func metricChecks(res *Result) []string {
	var failures []string
	take := res.Takes[len(res.Takes)-1]
	if err := take.Report.Check(); err != nil {
		failures = append(failures, err.Error())
	}
	if take.Report.SyncErrorMSP95 > 20 {
		failures = append(failures, "sync_error_ms_p95 exceeds 20 ms")
	}
	if take.Report.PacketRecoveryRate < 0.95 {
		failures = append(failures, "packet_recovery_rate below 0.95")
	}
	if take.Report.LatencyMSP95 > 250 {
		failures = append(failures, "latency_ms_p95 exceeds 250 ms")
	}
	return failures
}

func shortDigest(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

func round(v float64, places int) float64 {
	if math.IsInf(v, 0) || math.IsNaN(v) {
		return 0
	}
	scale := math.Pow(10, float64(places))
	return math.Round(v*scale) / scale
}

// ExitCodeFor classifies a result for the process exit status.
func ExitCodeFor(res *Result) int {
	if res == nil {
		return 3
	}
	return res.ExitCode
}
