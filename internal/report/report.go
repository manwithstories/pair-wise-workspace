package report

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"text/tabwriter"

	"subtool/internal/encsniff"
	"subtool/internal/subtitle"
	"subtool/internal/timebase"
	"subtool/internal/verify"
)

type FrameRow struct {
	Index        int    `json:"index"`
	InMS         int64  `json:"in_ms"`
	OutMS        int64  `json:"out_ms"`
	InFrame      int64  `json:"in_frame"`
	OutFrame     int64  `json:"out_frame"`
	OverlapGroup int    `json:"overlap_group,omitempty"`
	Reason       string `json:"reason,omitempty"`
}

type FrameReport struct {
	Input    string          `json:"input"`
	FPS      string          `json:"fps"`
	Encoding encsniff.Result `json:"encoding"`
	Frames   []FrameRow      `json:"frames"`
	Warnings []string        `json:"warnings,omitempty"`
}

type ProbeReport struct {
	Input         string          `json:"input"`
	Format        string          `json:"format"`
	Encoding      encsniff.Result `json:"encoding"`
	CuesTotal     int             `json:"cues_total"`
	OverlapGroups int             `json:"overlap_groups"`
	InvalidCues   int             `json:"invalid_cues"`
	Dropped       []verify.Drop   `json:"dropped"`
	Warnings      []string        `json:"warnings,omitempty"`
}

type ConvertReport struct {
	Input          string          `json:"input"`
	Output         string          `json:"output"`
	InFPS          string          `json:"in_fps"`
	OutFPS         string          `json:"out_fps"`
	Mode           string          `json:"mode"`
	Encoding       encsniff.Result `json:"encoding"`
	CuesTotal      int             `json:"cues_total"`
	Dropped        []verify.Drop   `json:"dropped"`
	DroppedTotal   int             `json:"dropped_total"`
	QuantizedMaxMS int64           `json:"quantized_max_ms"`
	Warnings       []string        `json:"warnings,omitempty"`
}

type RoundtripReport struct {
	Input          string          `json:"input"`
	Via            string          `json:"via"`
	InFPS          string          `json:"in_fps"`
	OutFPS         string          `json:"out_fps"`
	Mode           string          `json:"mode"`
	ToleranceMS    int64           `json:"tolerance_ms"`
	Encoding       encsniff.Result `json:"encoding"`
	ElapsedMS      int64           `json:"elapsed_ms"`
	Passed         bool            `json:"passed"`
	QuantizedMaxMS int64           `json:"quantized_max_ms"`
	Warnings       []string        `json:"warnings,omitempty"`
	verify.Result
}

func BuildFrames(input string, rate timebase.Rate, track *subtitle.Track) FrameReport {
	report := FrameReport{Input: input, FPS: rate.String(), Frames: make([]FrameRow, 0, len(track.Cues))}
	for _, cue := range track.Cues {
		reason := cue.Reason
		if cue.Invalid && reason == "" {
			reason = "invalid_cue"
		}
		report.Frames = append(report.Frames, FrameRow{
			Index: cue.Index, InMS: cue.StartMS, OutMS: cue.EndMS,
			InFrame: timebase.FrameForMS(cue.StartMS, rate), OutFrame: timebase.FrameForMS(cue.EndMS, rate),
			OverlapGroup: cue.OverlapGroup, Reason: reason,
		})
	}
	return report
}

func BuildProbe(input string, format subtitle.Format, enc encsniff.Result, track *subtitle.Track) ProbeReport {
	return ProbeReport{Input: input, Format: string(format), Encoding: enc,
		CuesTotal: len(track.Cues), OverlapGroups: subtitle.OverlapGroupCount(track.Cues),
		InvalidCues: subtitle.InvalidCount(track.Cues), Dropped: Drops(track)}
}

func Drops(track *subtitle.Track) []verify.Drop {
	items := make([]verify.Drop, 0)
	for _, cue := range track.Cues {
		if !cue.Invalid && cue.Reason == "" {
			continue
		}
		reason := cue.Reason
		if reason == "" {
			reason = "invalid_cue"
		}
		items = append(items, verify.Drop{Index: cue.Index, Reason: reason,
			StartMS: cue.StartMS, EndMS: cue.EndMS, Text: cue.Text})
	}
	return items
}

func WriteJSON(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	writer := bufio.NewWriterSize(file, 64*1024)
	encoder := json.NewEncoder(writer)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		_ = file.Close()
		return err
	}
	if err := writer.Flush(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func PrintProbe(out *os.File, value ProbeReport) {
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "FIELD\tVALUE")
	fmt.Fprintf(tw, "input\t%s\nformat\t%s\nencoding\t%s\nconfidence\t%.3f\n",
		value.Input, value.Format, value.Encoding.Name, value.Encoding.Confidence)
	fmt.Fprintf(tw, "cues_total\t%d\noverlap_groups\t%d\ninvalid_cues\t%d\n",
		value.CuesTotal, value.OverlapGroups, value.InvalidCues)
	for _, warning := range value.Warnings {
		fmt.Fprintf(tw, "warning\t%s\n", warning)
	}
	_ = tw.Flush()
}

func PrintFrames(out *os.File, value FrameReport) {
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	for _, warning := range value.Warnings {
		fmt.Fprintf(tw, "warning\t%s\n", warning)
	}
	fmt.Fprintln(tw, "INDEX\tIN_MS\tOUT_MS\tIN_FRAME\tOUT_FRAME\tOVERLAP")
	for _, row := range value.Frames {
		fmt.Fprintf(tw, "%d\t%d\t%d\t%d\t%d\t%d\n", row.Index, row.InMS, row.OutMS,
			row.InFrame, row.OutFrame, row.OverlapGroup)
	}
	_ = tw.Flush()
}

func PrintConvert(out *os.File, value ConvertReport) {
	fmt.Fprintf(out, "convert %s -> %s mode=%s cues=%d dropped=%d quantized_max_ms=%d encoding=%s(%.3f)\n",
		value.Input, value.Output, value.Mode, value.CuesTotal, value.DroppedTotal,
		value.QuantizedMaxMS, value.Encoding.Name, value.Encoding.Confidence)
	for _, item := range value.Dropped {
		fmt.Fprintf(out, "DROP index=%d reason=%s in_ms=%d out_ms=%d\n",
			item.Index, item.Reason, item.StartMS, item.EndMS)
	}
}

func PrintRoundtrip(out *os.File, value RoundtripReport) {
	fmt.Fprintf(out, "roundtrip via=%s mode=%s %s elapsed_ms=%d passed=%t\n",
		value.Via, value.Mode, value.Result.Summary(), value.ElapsedMS, value.Passed)
	for _, item := range value.Dropped {
		fmt.Fprintf(out, "DROP index=%d reason=%s\n", item.Index, item.Reason)
	}
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "INDEX\tIN_MS\tOUT_MS\tIN_FRAME\tOUT_FRAME\tDRIFT_MS\tREASON")
	for _, delta := range value.Deltas {
		fmt.Fprintf(tw, "%d\t%d\t%d\t%d\t%d\t%d\t%s\n", delta.Index, delta.InMS,
			delta.OutMS, delta.InFrame, delta.OutFrame, delta.DriftMS, delta.Reason)
	}
	_ = tw.Flush()
}
