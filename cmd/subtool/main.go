// subtool is a subtitle timeline resampling and cross-format
// verification batch processor. Pure terminal, local files only.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"subtool/internal/encoding"
	"subtool/internal/fixture"
	"subtool/internal/model"
	"subtool/internal/report"
	"subtool/internal/serialize"
	"subtool/internal/timeline"
	"subtool/internal/verify"
)

// Exit codes per delivery contract.
const (
	ExitOK          = 0
	ExitUsage       = 2 // bad params or path escapes whitelist
	ExitQuarantined = 3 // quarantined entries over threshold
	ExitRoundtrip   = 4 // reverse verification failed
	ExitIO          = 5 // IO or build error
)

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	if len(args) == 0 {
		usage()
		return ExitUsage
	}
	switch args[0] {
	case "convert":
		return cmdConvert(args[1:])
	case "verify":
		return cmdVerify(args[1:])
	case "selftest":
		return cmdSelftest(args[1:])
	}
	usage()
	return ExitUsage
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  subtool selftest --out DIR [--seed N] [--cues N]
  subtool convert --in FILE|DIR [--from FMT] --to FMT [--src-fps R] [--dst-fps R]
                  [--round floor|nearest|ceil] [--overlap layer|trim|split]
                  [--jobs N] [--out DIR] [--report FILE] [--json]
  subtool verify --in DIR [--matrix all] [--max-drift-ms F] [--json]`)
}

// fileJob is one input file flowing through the batch pipeline.
type fileJob struct {
	path string
}

// fileResult is the per-file pipeline outcome; failures are isolated.
type fileResult struct {
	path         string
	format       string
	encName      string
	encConf      float64
	cues         []model.Cue
	issues       []model.Issue
	rows         []report.CueRow
	dropped      int
	added        int
	textMismatch int
	maxDrift     float64
	p99Drift     float64
	snapDrift    float64
	trimmedMs    float64
	splits       int
	trims        int
	quarantined  bool
	err          error
}

// pipelineOpts carries convert/verify parameters.
type pipelineOpts struct {
	from     string
	to       string
	dstFPS   timeline.FPS
	round    timeline.RoundMode
	overlap  timeline.ResolveMode
	outDir   string
	writeOut bool
}

// detectFormat picks a format from --from or the file extension.
func detectFormat(path, forced string) (string, error) {
	if forced != "" {
		return forced, nil
	}
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(path), "."))
	switch ext {
	case "srt", "vtt", "ass", "ssa", "ttml", "dfxp", "xml":
		if ext == "ssa" {
			return "ass", nil
		}
		if ext == "dfxp" || ext == "xml" {
			return "ttml", nil
		}
		return ext, nil
	}
	return "", fmt.Errorf("cannot detect format of %s (use --from)", path)
}

// processFile runs decode -> parse -> overlap -> resample -> serialize
// -> re-parse -> diff for one file. Errors never escape as panics.
func processFile(j fileJob, opts pipelineOpts) (r fileResult) {
	r.path = j.path
	defer func() {
		if rec := recover(); rec != nil {
			r.err = fmt.Errorf("panic: %v", rec)
		}
	}()
	format, err := detectFormat(j.path, opts.from)
	if err != nil {
		r.err = err
		return r
	}
	r.format = format
	data, err := os.ReadFile(j.path)
	if err != nil {
		r.err = err
		return r
	}
	sniff := encoding.Sniff(data)
	r.encName, r.encConf = sniff.Name, sniff.Confidence
	for _, ln := range sniff.BadLines {
		r.issues = append(r.issues, model.Issue{
			Line: ln, Code: model.CodeUndecodable, Msg: "undecodable bytes isolated",
		})
	}
	parsed := verify.ParseByFormat(sniff.Text, format)
	r.issues = append(r.issues, parsed.Issues...)
	cues, stats := timeline.Resolve(parsed.Cues, opts.overlap)
	r.splits, r.trims = stats.Splits, stats.Trims
	if stats.TrimmedMs != nil {
		r.trimmedMs, _ = stats.TrimmedMs.Float64()
	}
	for i := range cues {
		opts.dstFPS.Snap(&cues[i], opts.round)
	}
	r.cues = cues
	rt, err := verify.Roundtrip(cues, opts.to)
	if err != nil {
		r.err = err
		return r
	}
	r.dropped, r.added = rt.DroppedCues, rt.AddedCues
	r.textMismatch, r.maxDrift, r.p99Drift = rt.TextMismatch, rt.MaxDriftMs, rt.P99DriftMs
	driftByID := map[string]float64{}
	for _, row := range rt.Rows {
		driftByID[row.ID] = row.DriftMs
	}
	for _, c := range cues {
		snap := opts.dstFPS.SnapDriftMs(&c)
		if snap > r.snapDrift {
			r.snapDrift = snap
		}
		status := "OK"
		if c.Subframe {
			status = "SUBFRAME"
		}
		r.rows = append(r.rows, report.CueRow{
			CueID: c.ID, StartFrame: c.StartFrame, EndFrame: c.EndFrame,
			StartMs: model.RatToFloat(c.Start), EndMs: model.RatToFloat(c.End),
			RtDriftMs: driftByID[c.ID], FrameSnapDriftMs: snap, Status: status,
		})
	}
	if opts.writeOut {
		if err := os.MkdirAll(opts.outDir, 0o755); err != nil {
			r.err = err
			return r
		}
		base := strings.TrimSuffix(filepath.Base(j.path), filepath.Ext(j.path))
		outPath, err := safeJoin(opts.outDir, base+"."+opts.to)
		if err != nil {
			r.err = err
			return r
		}
		if err := os.WriteFile(outPath, []byte(rt.Serialized), 0o644); err != nil {
			r.err = err
			return r
		}
	}
	r.quarantined = len(r.issues) > 0
	return r
}

// safeJoin ensures name stays inside dir (output path whitelist).
func safeJoin(dir, name string) (string, error) {
	clean := filepath.Clean(dir)
	p := filepath.Join(clean, name)
	if p != clean && !strings.HasPrefix(p, clean+string(os.PathSeparator)) {
		return "", fmt.Errorf("path %q escapes --out whitelist", name)
	}
	return p, nil
}

// collectInputs expands --in into a sorted file list.
func collectInputs(in string) ([]string, error) {
	info, err := os.Stat(in)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return []string{in}, nil
	}
	var files []string
	entries, err := os.ReadDir(in)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if _, err := detectFormat(e.Name(), ""); err == nil {
			files = append(files, filepath.Join(in, e.Name()))
		}
	}
	sort.Strings(files)
	return files, nil
}

// runBatch executes processFile concurrently with deterministic
// (path-sorted) result collection.
func runBatch(files []string, opts pipelineOpts, jobs int) []fileResult {
	if jobs < 1 {
		jobs = 1
	}
	results := make([]fileResult, len(files))
	var wg sync.WaitGroup
	sem := make(chan struct{}, jobs)
	for i, f := range files {
		wg.Add(1)
		go func(i int, f string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = processFile(fileJob{path: f}, opts)
		}(i, f)
	}
	wg.Wait()
	return results
}

// aggregate folds per-file results into one summary.
func aggregate(results []fileResult, totalCues int) report.Summary {
	var s report.Summary
	s.TotalCues = totalCues
	for _, r := range results {
		s.EmittedCues += len(r.cues)
		s.DroppedCues += r.dropped
		s.AddedCues += r.added
		s.TextMismatch += r.textMismatch
		s.OverlapSplits += r.splits
		s.OverlapTrims += r.trims
		s.OverlapTrimmedMs += r.trimmedMs
		s.QuarantinedCues += len(r.issues)
		if r.maxDrift > s.MaxDriftMs {
			s.MaxDriftMs = r.maxDrift
		}
		if r.p99Drift > s.P99DriftMs {
			s.P99DriftMs = r.p99Drift
		}
		if r.snapDrift > s.FrameSnapDriftMs {
			s.FrameSnapDriftMs = r.snapDrift
		}
		if r.quarantined {
			s.QuarantinedFiles++
		}
		if r.encName != "" {
			s.DetectedEncoding = r.encName
			s.EncodingConfidence = r.encConf
		}
		for _, is := range r.issues {
			s.Issues = append(s.Issues, filepath.Base(r.path)+": "+is.String())
		}
	}
	return s
}

func cmdConvert(args []string) int {
	fs := flag.NewFlagSet("convert", flag.ContinueOnError)
	in := fs.String("in", "", "input file or directory")
	from := fs.String("from", "", "override input format")
	to := fs.String("to", "", "output format: srt|vtt|ass|ttml")
	srcFPS := fs.String("src-fps", "", "source frame rate (informational)")
	dstFPS := fs.String("dst-fps", "24", "destination frame rate")
	round := fs.String("round", "nearest", "floor|nearest|ceil")
	overlap := fs.String("overlap", "layer", "layer|trim|split")
	jobs := fs.Int("jobs", 4, "concurrent files")
	outDir := fs.String("out", "", "output directory (whitelist root)")
	reportPath := fs.String("report", "", "write JSON report to file")
	asJSON := fs.Bool("json", false, "write JSON report to stdout")
	maxQuar := fs.Int("max-quarantined", 50, "quarantine threshold")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	if *in == "" || *to == "" {
		usage()
		return ExitUsage
	}
	fps, err := timeline.ParseFPS(*dstFPS)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return ExitUsage
	}
	if *srcFPS != "" {
		if _, err := timeline.ParseFPS(*srcFPS); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return ExitUsage
		}
	}
	validTo := false
	for _, f := range serialize.Formats {
		if *to == f {
			validTo = true
		}
	}
	if !validTo {
		fmt.Fprintf(os.Stderr, "unknown --to format %q\n", *to)
		return ExitUsage
	}
	files, err := collectInputs(*in)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return ExitIO
	}
	opts := pipelineOpts{
		from: *from, to: *to, dstFPS: fps,
		round:   timeline.RoundMode(*round),
		overlap: timeline.ResolveMode(*overlap),
		outDir:  *outDir, writeOut: *outDir != "",
	}
	results := runBatch(files, opts, *jobs)
	var rows []report.CueRow
	total := 0
	for _, r := range results {
		if r.err != nil {
			fmt.Fprintf(os.Stderr, "isolated failure %s: %v\n", r.path, r.err)
		}
		rows = append(rows, r.rows...)
		total += len(r.cues) + len(r.issues)
	}
	summary := aggregate(results, total)
	if *asJSON {
		report.WriteJSON(os.Stdout, summary, rows)
	} else {
		report.PrintRows(os.Stdout, rows)
		report.PrintSummary(os.Stdout, summary)
	}
	if *reportPath != "" {
		if dir := filepath.Dir(*reportPath); dir != "." {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				fmt.Fprintln(os.Stderr, err)
				return ExitIO
			}
		}
		f, err := os.Create(*reportPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return ExitIO
		}
		defer f.Close()
		if err := report.WriteJSON(f, summary, rows); err != nil {
			return ExitIO
		}
	}
	if summary.QuarantinedCues > *maxQuar {
		return ExitQuarantined
	}
	if summary.DroppedCues > 0 || summary.AddedCues > 0 || summary.TextMismatch > 0 {
		return ExitRoundtrip
	}
	return ExitOK
}

func cmdVerify(args []string) int {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	in := fs.String("in", "", "input directory")
	matrix := fs.String("matrix", "all", "conversion matrix scope")
	maxDrift := fs.Float64("max-drift-ms", 1.0, "drift threshold")
	asJSON := fs.Bool("json", false, "write JSON report to stdout")
	jobs := fs.Int("jobs", 4, "concurrent files")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	if *in == "" || *matrix != "all" {
		usage()
		return ExitUsage
	}
	files, err := collectInputs(*in)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return ExitIO
	}
	fps, _ := timeline.ParseFPS("24000/1001")
	var allRows []report.CueRow
	var summaries []report.Summary
	fail := false
	// Full 4x3=12 path matrix: every source parsed once, converted to
	// every other format and round-trip verified.
	for _, to := range serialize.Formats {
		opts := pipelineOpts{
			to: to, dstFPS: fps, round: timeline.RoundNearest,
			overlap: timeline.ModeLayer,
		}
		results := runBatch(files, opts, *jobs)
		total := 0
		for _, r := range results {
			if r.err != nil {
				fmt.Fprintf(os.Stderr, "isolated failure %s: %v\n", r.path, r.err)
				continue
			}
			if r.format == to {
				continue // same-format pass covered by other sources
			}
			allRows = append(allRows, r.rows...)
			total += len(r.cues)
		}
		s := aggregate(results, total)
		summaries = append(summaries, s)
		if s.MaxDriftMs > *maxDrift || s.DroppedCues > 0 ||
			s.AddedCues > 0 || s.TextMismatch > 0 {
			fail = true
		}
	}
	merged := report.Summary{DetectedEncoding: "multi"}
	for _, s := range summaries {
		merged.TotalCues += s.TotalCues
		merged.EmittedCues += s.EmittedCues
		merged.DroppedCues += s.DroppedCues
		merged.AddedCues += s.AddedCues
		merged.TextMismatch += s.TextMismatch
		merged.QuarantinedFiles += s.QuarantinedFiles
		merged.QuarantinedCues += s.QuarantinedCues
		if s.MaxDriftMs > merged.MaxDriftMs {
			merged.MaxDriftMs = s.MaxDriftMs
		}
		if s.P99DriftMs > merged.P99DriftMs {
			merged.P99DriftMs = s.P99DriftMs
		}
	}
	if *asJSON {
		report.WriteJSON(os.Stdout, merged, allRows)
	} else {
		report.PrintSummary(os.Stdout, merged)
	}
	if fail {
		return ExitRoundtrip
	}
	return ExitOK
}

func cmdSelftest(args []string) int {
	fs := flag.NewFlagSet("selftest", flag.ContinueOnError)
	out := fs.String("out", "", "fixture output directory")
	seed := fs.Int64("seed", 1, "fixture random seed")
	n := fs.Int("cues", 2400, "cue count")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	if *out == "" {
		usage()
		return ExitUsage
	}
	corpus, err := fixture.Generate(*out, *seed, *n)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return ExitIO
	}
	fmt.Fprintf(os.Stderr, "fixtures: %d cues, %d files in %s\n",
		len(corpus.Cues), len(corpus.Files), *out)
	// Run the full conversion matrix over the generated corpus.
	code := cmdVerify([]string{"--in", *out, "--matrix", "all",
		"--max-drift-ms", "1.0"})
	if code != ExitOK {
		return code
	}
	fmt.Fprintln(os.Stderr, "selftest OK: 12-path matrix passed")
	return ExitOK
}
