// Command mediacheck checks esports VOD media: MP4 box/sample-table
// integrity, caption timing, timecode conversion and resumable
// chunked hashing, all on a shared nanosecond timeline.
//
// Exit codes: 0 = metrics pass; 2 = input corrupt but report
// generated; 1 = run or acceptance failure.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"mediacheck/internal/captions"
	"mediacheck/internal/fault"
	"mediacheck/internal/pipeline"
	"mediacheck/internal/stream"
	"mediacheck/internal/timeline"
)

// report is the JSON summary printed at the end of every path.
type report struct {
	Path             string                `json:"path"`
	Input            string                `json:"input"`
	Tracks           int                   `json:"tracks"`
	CueCount         int                   `json:"cue_count"`
	SegmentCount     int                   `json:"segment_count"`
	Segments         []pipeline.SegmentHash `json:"segments"`
	CorruptRanges    []stream.Range        `json:"corrupt_ranges"`
	RoundtripRate    float64               `json:"roundtrip_rate"`
	KeyframeHitRate  float64               `json:"keyframe_hit_rate"`
	ResumeMatches    bool                  `json:"resume_matches"`
	PeakBufferBytes  int64                 `json:"peak_buffer_bytes"`
	PeakBufferBudget int64                 `json:"peak_buffer_budget"`
	FaultCounts      map[fault.Category]int `json:"fault_counts"`
}

func main() {
	var (
		input      = flag.String("input", "", "MP4 input file")
		captionArg = flag.String("captions", "", "SRT/WebVTT caption file")
		fps        = flag.String("timecode-fps", "30000/1001", "frame rate: 24, 25, 30 or 30000/1001")
		queue      = flag.Int("queue", 4, "pipeline queue capacity in chunks (1..64)")
		chunk      = int64(0)
		checkpoint = flag.String("checkpoint", "", "checkpoint state file for resume")
		selftest   = flag.Bool("selftest", false, "generate fixtures and run all acceptance paths")
		reportFmt  = flag.String("report", "json", "report format: json")
		tmpdir     = flag.String("tmpdir", "./tmp", "directory for generated fixtures")
	)
	flag.Int64Var(&chunk, "chunk", 1<<20, "chunk size in bytes")
	flag.Parse()
	if *reportFmt != "json" {
		fmt.Fprintln(os.Stderr, "only -report json is supported")
		os.Exit(1)
	}
	rate, err := timeline.ParseRate(*fps)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	cfg := pipeline.Config{QueueCap: *queue, ChunkSize: chunk}

	if *selftest {
		os.Exit(runSelftest(*tmpdir, rate, cfg))
	}
	if *input == "" {
		fmt.Fprintln(os.Stderr, "-input is required (or use -selftest)")
		os.Exit(1)
	}
	// Demo entry self-generates fixtures when the input is missing.
	if _, err := os.Stat(*input); os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "input %s not found, generating fixtures in %s\n", *input, *tmpdir)
		if err := generateFixtures(*tmpdir); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	os.Exit(runNormal(*input, *captionArg, rate, cfg, *checkpoint))
}

// runNormal executes the normal path: scan, parse captions, pipeline.
func runNormal(input, captionPath string, rate timeline.Rate,
	cfg pipeline.Config, checkpoint string) int {
	rep, faults, err := analyze("normal", input, captionPath, rate, cfg, checkpoint)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	printReport(rep, faults)
	if faults.Len() > 0 {
		return 2 // corrupt input, report still generated
	}
	return 0
}

// analyze runs one full pass and returns the report plus all faults.
func analyze(path, input, captionPath string, rate timeline.Rate,
	cfg pipeline.Config, checkpoint string) (*report, *fault.List, error) {
	rep := &report{Path: path, Input: input, FaultCounts: map[fault.Category]int{}}
	faults := &fault.List{}

	info := scanMP4File(input, faults)
	rep.Tracks = len(info.Tracks)
	rep.CorruptRanges = corruptRanges(info)

	var cues []captions.Cue
	if captionPath != "" {
		res := parseCaptionFile(captionPath, faults)
		if res != nil {
			cues = res.Cues
		}
		rep.CueCount = len(cues)
	}

	f, err := os.Open(input)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	res, err := pipeline.Run(context.Background(), f, cfg, checkpoint, nil)
	if err != nil {
		return nil, nil, err
	}
	faults.Errs = append(faults.Errs, res.Faults.Errs...)
	rep.Segments = res.Segments
	rep.SegmentCount = len(res.Segments)
	rep.ResumeMatches = res.ResumeMatches
	rep.PeakBufferBytes = res.PeakBufferBytes
	rep.PeakBufferBudget = int64(cfg.QueueCap)*cfg.ChunkSize + pipeline.ParseBufferBytes

	// Keyframes on the shared timeline: round-trip and hit metrics.
	var keys []int64
	for _, tr := range info.Tracks {
		keys = append(keys, tr.Keyframes...)
	}
	rep.RoundtripRate = timecodeRoundtrip(keys, rate)
	rep.KeyframeHitRate = keyframeSegmentHitRate(keys, rep.CorruptRanges)
	rep.FaultCounts = faults.ByCategory()
	return rep, faults, nil
}

// scanMP4File opens and scans an MP4, converting fatal problems into
// process-level errors only when nothing can be read at all.
func scanMP4File(path string, faults *fault.List) *stream.MP4Info {
	f, err := os.Open(path)
	if err != nil {
		faults.Addf(fault.CatStream, -1, false, "open: %v", err)
		return &stream.MP4Info{Faults: &fault.List{}}
	}
	defer f.Close()
	st, _ := f.Stat()
	info := stream.ScanMP4(f, st.Size())
	faults.Errs = append(faults.Errs, info.Faults.Errs...)
	return info
}

// corruptRanges maps the unread tail of a truncated MP4 to a
// closed-open byte range.
func corruptRanges(info *stream.MP4Info) []stream.Range {
	if info.TotalBytes > 0 && info.TrustedBytes < info.TotalBytes {
		return []stream.Range{{Start: info.TrustedBytes, End: info.TotalBytes}}
	}
	return nil
}

// parseCaptionFile picks the SRT or VTT parser by extension/content.
func parseCaptionFile(path string, faults *fault.List) *captions.Result {
	f, err := os.Open(path)
	if err != nil {
		faults.Addf(fault.CatCaption, -1, false, "open captions: %v", err)
		return nil
	}
	defer f.Close()
	var res *captions.Result
	if strings.HasSuffix(strings.ToLower(path), ".srt") {
		res = captions.ParseSRT(f)
	} else {
		res = captions.ParseVTT(f)
	}
	faults.Errs = append(faults.Errs, res.Faults.Errs...)
	return res
}

// timecodeRoundtrip formats each timestamp as a timecode and parses it
// back; the rate of exact matches is the round-trip rate.
func timecodeRoundtrip(ns []int64, rate timeline.Rate) float64 {
	if len(ns) == 0 {
		return 1
	}
	drop := rate.DropFrameAllowed()
	hits := 0
	for _, t := range ns {
		s := timeline.Format(t, rate, drop)
		tc, err := timeline.ParseTimecode(s, rate, 0)
		if err == nil && tc.Nanos == t {
			hits++
		}
	}
	return float64(hits) / float64(len(ns))
}

// keyframeSegmentHitRate is the fraction of GOP segments free of
// corrupt ranges on the shared timeline.
func keyframeSegmentHitRate(keys []int64, corrupt []stream.Range) float64 {
	if len(keys) == 0 {
		return 1
	}
	duration := keys[len(keys)-1] + 1
	segs := stream.BuildSegments(keys, duration, corrupt)
	if len(segs) == 0 {
		return 1
	}
	clean := 0
	for _, s := range segs {
		if !s.Corrupted {
			clean++
		}
	}
	return float64(clean) / float64(len(segs))
}

func printReport(rep *report, faults *fault.List) {
	printReportTo(os.Stdout, rep, faults)
}

func printReportTo(w io.Writer, rep *report, faults *fault.List) {
	out, _ := json.MarshalIndent(rep, "", "  ")
	fmt.Fprintln(w, string(out))
	if faults.Len() > 0 {
		fmt.Fprintf(os.Stderr, "faults by category: %v\n", faults.ByCategory())
		for _, e := range faults.Errs {
			fmt.Fprintln(os.Stderr, "  ", e)
		}
	}
}

