package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"dashpack/internal/fixture"
	"dashpack/internal/manifest"
	"dashpack/internal/packager"
	"dashpack/internal/verify"
)

type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }

func (s *stringList) Set(value string) error {
	for _, item := range strings.Split(value, ",") {
		if item != "" {
			*s = append(*s, item)
		}
	}
	return nil
}

type packOutput struct {
	OK               bool        `json:"ok"`
	Representations  int         `json:"representations"`
	Periods          int         `json:"periods"`
	Segments         int         `json:"segments"`
	ExpandedExamples []string    `json:"expanded_examples"`
	MaxDeviationMS   json.Number `json:"max_deviation_ms"`
	Gaps             int         `json:"gaps"`
	Overlaps         int         `json:"overlaps"`
}

type verifyOutput struct {
	Representations int         `json:"representations"`
	CheckedSegments int         `json:"checked_segments"`
	MaxDeviationMS  json.Number `json:"max_deviation_ms"`
	MissingSegments int         `json:"missing_segments"`
	Gaps            int         `json:"gaps"`
	Overlaps        int         `json:"overlaps"`
}

type genOutput struct {
	OK              bool   `json:"ok"`
	Representations int    `json:"representations"`
	Segments        int    `json:"segments"`
	Seed            int64  `json:"seed"`
	Out             string `json:"out"`
}

type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Details any    `json:"details"`
}

type errorEnvelope struct {
	OK    bool      `json:"ok"`
	Error errorBody `json:"error"`
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return fail(stderr, manifest.NewError("PARAM_INVALID",
			"expected subcommand: gen, pack, expand, or verify", 2, nil))
	}
	switch args[0] {
	case "gen":
		return runGen(args[1:], stdout, stderr)
	case "pack":
		return runPack(args[1:], stdout, stderr)
	case "expand":
		return runExpand(args[1:], stdout, stderr)
	case "verify":
		return runVerify(args[1:], stdout, stderr)
	default:
		return fail(stderr, manifest.NewError("PARAM_INVALID",
			fmt.Sprintf("unknown subcommand %q", args[0]), 2, nil))
	}
}

func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

func flagError(err error) error {
	return manifest.NewError("PARAM_INVALID", err.Error(), 2, nil)
}

func runGen(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("gen")
	out := fs.String("out", "", "output fixture directory")
	seed := fs.Int64("seed", 0, "deterministic seed")
	video := fs.Int("video", 4, "video representation count")
	audio := fs.Int("audio", 2, "audio representation count")
	segments := fs.Int("segments", 120, "segments per representation")
	jsonOutput := fs.Bool("json", false, "emit JSON")
	var injections stringList
	fs.Var(&injections, "inject", "fault injection name:value")
	if err := fs.Parse(args); err != nil {
		return fail(stderr, flagError(err))
	}
	if fs.NArg() != 0 {
		return fail(stderr, flagError(fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))))
	}
	if *out == "" {
		return fail(stderr, manifest.NewError("PARAM_INVALID", "--out is required", 2, nil))
	}
	parsedInjections, err := fixture.ParseInjections(injections)
	if err != nil {
		return fail(stderr, err)
	}
	result, err := fixture.Generate(fixture.Options{
		Out: *out, Seed: *seed, Video: *video, Audio: *audio,
		Segments: *segments, Injections: parsedInjections,
	})
	if err != nil {
		return fail(stderr, err)
	}
	output := genOutput{OK: true, Representations: result.Representations,
		Segments: result.Segments, Seed: result.Seed, Out: result.Out}
	if *jsonOutput {
		writeJSONLine(stdout, output)
	} else {
		fmt.Fprintf(stdout, "generated %d representations and %d segments in %s\n",
			result.Representations, result.Segments, result.Out)
	}
	return 0
}

func runPack(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("pack")
	input := fs.String("in", "", "fixture input directory")
	out := fs.String("out", "", "packaged output directory")
	segmentMS := fs.Int64("segment-ms", 2000, "common segment grid in milliseconds")
	toleranceMS := fs.Int64("tolerance-ms", 10, "alignment tolerance in milliseconds")
	jsonOutput := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return fail(stderr, flagError(err))
	}
	if fs.NArg() != 0 {
		return fail(stderr, flagError(fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))))
	}
	if *input == "" || *out == "" {
		return fail(stderr, manifest.NewError("PARAM_INVALID",
			"--in and --out are required", 2, nil))
	}
	result, err := packager.Pack(packager.Options{
		Input: *input, Out: *out, SegmentMS: *segmentMS, ToleranceMS: *toleranceMS,
	})
	if err != nil {
		return fail(stderr, err)
	}
	output := packOutput{
		OK: true, Representations: result.Representations, Periods: result.Periods,
		Segments: result.Segments, ExpandedExamples: result.ExpandedExamples,
		MaxDeviationMS: json.Number(result.MaxDeviationMS),
		Gaps:           result.Gaps, Overlaps: result.Overlaps,
	}
	if *jsonOutput {
		writeJSONLine(stdout, output)
	} else {
		fmt.Fprintf(stdout, "packed %d representations, %d segments; max deviation %s ms\n",
			result.Representations, result.Segments, result.MaxDeviationMS)
	}
	return 0
}

type expandedEntry struct {
	RepresentationID string `json:"representation"`
	Number           int64  `json:"number"`
	Time             int64  `json:"time"`
	Duration         int64  `json:"duration"`
	URL              string `json:"url"`
}

type expandedReport struct {
	Representations int             `json:"representations"`
	Segments        int             `json:"segments"`
	Entries         []expandedEntry `json:"entries"`
}

type expandOutput struct {
	OK              bool     `json:"ok"`
	Representations int      `json:"representations"`
	Segments        int      `json:"segments"`
	Head            []string `json:"head"`
	Tail            []string `json:"tail"`
}

func runExpand(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("expand")
	mpdPath := fs.String("mpd", "", "manifest path")
	input := fs.String("in", "", "fixture input root")
	out := fs.String("out", "", "optional report file or directory")
	jsonOutput := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return fail(stderr, flagError(err))
	}
	if fs.NArg() != 0 {
		return fail(stderr, flagError(fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))))
	}
	if *mpdPath == "" || *input == "" {
		return fail(stderr, manifest.NewError("PARAM_INVALID",
			"--mpd and --in are required", 2, nil))
	}
	mpd, err := manifest.ParseFile(*mpdPath)
	if err != nil {
		return fail(stderr, manifest.NewError("MPD_PARSE",
			fmt.Sprintf("read manifest: %v", err), 4, nil))
	}
	if err := mpd.Validate(); err != nil {
		return fail(stderr, err)
	}
	if err := validateManifestPaths(mpd, *input); err != nil {
		return fail(stderr, err)
	}
	all, err := manifest.ExpandMPD(mpd)
	if err != nil {
		return fail(stderr, manifest.NewError("MPD_INVALID", err.Error(), 4, nil))
	}
	headCount, tailCount := 3, 3
	if headCount > len(all) {
		headCount = len(all)
	}
	if tailCount > len(all) {
		tailCount = len(all)
	}
	head := manifest.URLs(all[:headCount])
	tail := manifest.URLs(all[len(all)-tailCount:])
	output := expandOutput{
		OK: true, Representations: len(mpd.AllRepresentations()),
		Segments: len(all), Head: head, Tail: tail,
	}
	report := expandedReport{Representations: output.Representations, Segments: len(all)}
	for _, segment := range all {
		report.Entries = append(report.Entries, expandedEntry{
			RepresentationID: segment.RepresentationID, Number: segment.Number,
			Time: segment.Time, Duration: segment.Duration, URL: segment.URL,
		})
	}
	if *out != "" {
		data, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			return fail(stderr, err)
		}
		if err := writeOptionalReport(*out, "expanded.json", append(data, '\n')); err != nil {
			return fail(stderr, err)
		}
	}
	if *jsonOutput {
		writeJSONLine(stdout, output)
	} else {
		fmt.Fprintf(stdout, "expanded %d segments across %d representations\n",
			len(all), output.Representations)
		for _, url := range head {
			fmt.Fprintf(stdout, "head %s\n", url)
		}
		for _, url := range tail {
			fmt.Fprintf(stdout, "tail %s\n", url)
		}
	}
	return 0
}

func validateManifestPaths(mpd *manifest.MPD, input string) error {
	for i := range mpd.Periods {
		for j := range mpd.Periods[i].AdaptationSets {
			for k := range mpd.Periods[i].AdaptationSets[j].Representations {
				repr := &mpd.Periods[i].AdaptationSets[j].Representations[k]
				template := repr.SegmentTemplate
				if template.Initialization != "" {
					url := manifest.ExpandTemplate(template.Initialization, manifest.ExpandContext{
						RepresentationID: repr.ID, Bandwidth: repr.Bandwidth,
					})
					if _, err := manifest.SafeJoin(input, url); err != nil {
						return manifest.NewError("PATH_ESCAPE", err.Error(), 4, url)
					}
				}
				segments, err := manifest.ExpandRepresentation(repr)
				if err != nil {
					return manifest.NewError("MPD_INVALID", err.Error(), 4, repr.ID)
				}
				for _, segment := range segments {
					if _, err := manifest.SafeJoin(input, segment.URL); err != nil {
						return manifest.NewError("PATH_ESCAPE", err.Error(), 4, segment.URL)
					}
				}
			}
		}
	}
	return nil
}

func runVerify(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("verify")
	mpdPath := fs.String("mpd", "", "manifest path")
	input := fs.String("in", "", "fixture input root")
	toleranceMS := fs.Int64("tolerance-ms", 10, "alignment tolerance in milliseconds")
	out := fs.String("out", "", "optional report file or directory")
	jsonOutput := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return fail(stderr, flagError(err))
	}
	if fs.NArg() != 0 {
		return fail(stderr, flagError(fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))))
	}
	if *mpdPath == "" || *input == "" {
		return fail(stderr, manifest.NewError("PARAM_INVALID",
			"--mpd and --in are required", 2, nil))
	}
	result, err := verify.Verify(verify.Options{
		ManifestPath: *mpdPath, Input: *input, ToleranceMS: *toleranceMS,
	})
	if err != nil {
		return fail(stderr, err)
	}
	output := verifyOutput{
		Representations: result.Representations, CheckedSegments: result.CheckedSegments,
		MaxDeviationMS:  json.Number(result.MaxDeviationMS),
		MissingSegments: result.MissingSegments, Gaps: result.Gaps, Overlaps: result.Overlaps,
	}
	compact, err := json.Marshal(output)
	if err != nil {
		return fail(stderr, err)
	}
	compact = append(compact, '\n')
	if *out != "" {
		data, err := json.MarshalIndent(output, "", "  ")
		if err != nil {
			return fail(stderr, err)
		}
		if err := writeOptionalReport(*out, "verify.json", append(data, '\n')); err != nil {
			return fail(stderr, err)
		}
	}
	if *jsonOutput {
		_, _ = stdout.Write(compact)
	} else {
		fmt.Fprintf(stdout, "verified %d representations and %d segments; max deviation %s ms\n",
			result.Representations, result.CheckedSegments, result.MaxDeviationMS)
	}
	return 0
}

func writeOptionalReport(out, defaultName string, data []byte) error {
	info, err := os.Stat(out)
	path := out
	switch {
	case err == nil && info.IsDir():
		path = filepath.Join(out, defaultName)
	case err != nil && !errors.Is(err, os.ErrNotExist):
		return err
	case filepath.Ext(out) == "":
		if err := os.MkdirAll(out, 0o755); err != nil {
			return err
		}
		path = filepath.Join(out, defaultName)
	default:
		if parent := filepath.Dir(path); parent != "." {
			if err := os.MkdirAll(parent, 0o755); err != nil {
				return err
			}
		}
	}
	return os.WriteFile(path, data, 0o644)
}

func writeJSONLine(w io.Writer, value any) {
	data, err := json.Marshal(value)
	if err != nil {
		return
	}
	_, _ = w.Write(append(data, '\n'))
}

func fail(stderr io.Writer, err error) int {
	var coded *manifest.Error
	if !errors.As(err, &coded) {
		var validation *fixture.ValidationError
		if errors.As(err, &validation) {
			coded = manifest.NewError("PARAM_INVALID", validation.Message, 2, nil)
		} else {
			coded = manifest.NewError("IO_ERROR", err.Error(), 1, nil)
		}
	}
	exit := coded.Exit
	if exit == 0 {
		exit = 1
	}
	envelope := errorEnvelope{
		OK:    false,
		Error: errorBody{Code: coded.Code, Message: coded.Message, Details: coded.Details},
	}
	data, marshalErr := json.Marshal(envelope)
	if marshalErr != nil {
		fmt.Fprintf(stderr, `{"ok":false,"error":{"code":"INTERNAL_ERROR","message":%q,"details":null}}`+"\n",
			marshalErr.Error())
		return 1
	}
	_, _ = stderr.Write(append(data, '\n'))
	return exit
}
