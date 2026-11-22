// Command mediacore runs the demo media pipeline from the terminal.
//
//	mediacore demo --seed 42 --resume-dir .demo-state --report stdout
//	    [--scenario normal|corrupt|bad-input]
//
// Exit codes: 0 success, 2 input quality failure, 3 unrecoverable
// state or I/O error.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"mediacore/internal/clock"
	"mediacore/internal/demo"
	"mediacore/internal/fixture"
	"mediacore/internal/manifest"
	"mediacore/internal/pipeline"
	"mediacore/internal/provenance"
	"mediacore/internal/store"
)

const (
	exitOK           = 0
	exitInputQuality = 2
	exitUnrecoverable = 3
)

type errorReport struct {
	ErrorCode      string `json:"error_code"`
	Recoverable    bool   `json:"recoverable"`
	RecoveredNodes int    `json:"recovered_nodes,omitempty"`
	Detail         string `json:"detail,omitempty"`
}

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 || args[0] != "demo" {
		fmt.Fprintln(os.Stderr, "usage: mediacore demo --seed N --resume-dir DIR --report stdout [--scenario S]")
		return exitUnrecoverable
	}
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	seed := fs.Int64("seed", 42, "deterministic fixture seed")
	resumeDir := fs.String("resume-dir", ".demo-state", "state directory")
	report := fs.String("report", "stdout", "report destination (stdout)")
	scenario := fs.String("scenario", "normal", "normal|corrupt|bad-input")
	if err := fs.Parse(args[1:]); err != nil {
		return exitUnrecoverable
	}
	if *report != "stdout" {
		return fail(errorReport{ErrorCode: string(clock.CodeManifestInvalid),
			Recoverable: false, Detail: "only --report stdout is supported"}, exitUnrecoverable)
	}

	fixtureDir := filepath.Join(*resumeDir, "fixtures")
	fix, err := fixture.Generate(fixtureDir, *seed)
	if err != nil {
		return fail(errorReport{ErrorCode: string(clock.CodeIO), Recoverable: false,
			Detail: err.Error()}, exitUnrecoverable)
	}
	params := demo.DefaultParams(*seed, fixtureDir)

	switch *scenario {
	case "bad-input":
		return runBadInput(fixtureDir, fix)
	case "normal", "corrupt":
	default:
		return fail(errorReport{ErrorCode: string(clock.CodeManifestInvalid),
			Recoverable: false, Detail: "unknown scenario " + *scenario}, exitUnrecoverable)
	}

	st, err := store.Open(filepath.Join(*resumeDir, "checkpoints"))
	if err != nil {
		return fail(errorReport{ErrorCode: string(clock.CodeIO), Recoverable: false,
			Detail: err.Error()}, exitUnrecoverable)
	}
	inputDigest, err := demo.InputDigest(fixtureDir, fix)
	if err != nil {
		return fail(errorReport{ErrorCode: string(clock.CodeIO), Recoverable: false,
			Detail: err.Error()}, exitUnrecoverable)
	}
	newCtx := func() *pipeline.Context {
		return &pipeline.Context{
			Store:        st,
			Prov:         provenance.Open(filepath.Join(*resumeDir, "provenance.jsonl")),
			InputDigest:  inputDigest,
			ParamsDigest: demo.ParamsDigest(params),
		}
	}
	runner, err := pipeline.New(demo.Nodes(params, fix))
	if err != nil {
		return fail(errorReport{ErrorCode: string(clock.CodeUnrecoverable), Recoverable: false,
			Detail: err.Error()}, exitUnrecoverable)
	}

	if *scenario == "corrupt" {
		if _, err := runner.Run(newCtx()); err != nil {
			return failPipeline(err)
		}
		if err := corruptCheckpoint(*resumeDir, "cut"); err != nil {
			return fail(errorReport{ErrorCode: string(clock.CodeIO), Recoverable: false,
				Detail: err.Error()}, exitUnrecoverable)
		}
	}

	ctx := newCtx()
	sum, err := runner.Run(ctx)
	if err != nil {
		return failPipeline(err)
	}
	rep := ctx.Get("report").(*demo.ReportResult)
	summary := rep.Summary
	summary.RecoveredNodes = sum.RecoveredNodes
	summary.DuplicateCommits = sum.DuplicateCommits
	summary.FirstMissingDependency = "none"
	if sum.FirstMissingDependency != "" {
		summary.FirstMissingDependency = sum.FirstMissingDependency
	}

	if *scenario == "corrupt" {
		emit(errorReport{
			ErrorCode:      string(clock.CodeCheckpointCorrupt),
			Recoverable:    true,
			RecoveredNodes: sum.RecoveredNodes,
			Detail:         "recovered at node " + sum.RecoveryPosition,
		})
	}
	emit(summary)
	return exitOK
}

// runBadInput parses a manifest with a negative EXTINF duration and
// reports the input quality failure.
func runBadInput(dir string, fix *fixture.Result) int {
	bad := "#EXTM3U\n#EXT-X-TARGETDURATION:2\n#EXTINF:-1.5,\nmedia/v1-1.m4s\n"
	if err := os.WriteFile(filepath.Join(dir, "bad.m3u8"), []byte(bad), 0o644); err != nil {
		return fail(errorReport{ErrorCode: string(clock.CodeIO), Recoverable: false,
			Detail: err.Error()}, exitUnrecoverable)
	}
	f, err := os.Open(filepath.Join(dir, "bad.m3u8"))
	if err != nil {
		return fail(errorReport{ErrorCode: string(clock.CodeIO), Recoverable: false,
			Detail: err.Error()}, exitUnrecoverable)
	}
	defer f.Close()
	_, perr := manifest.ParseHLS(f, fix.VideoMeta)
	if perr == nil {
		return fail(errorReport{ErrorCode: string(clock.CodeInputQuality), Recoverable: false,
			Detail: "bad input was not rejected"}, exitInputQuality)
	}
	return fail(errorReport{
		ErrorCode:   string(clock.CodeOf(perr)),
		Recoverable: false,
		Detail:      perr.Error(),
	}, exitInputQuality)
}

// corruptCheckpoint flips one byte inside a committed checkpoint.
func corruptCheckpoint(resumeDir, node string) error {
	p := filepath.Join(resumeDir, "checkpoints", node+".json")
	data, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	if len(data) == 0 {
		return fmt.Errorf("checkpoint %s is empty", node)
	}
	data[len(data)/2] ^= 0xFF
	return os.WriteFile(p, data, 0o644)
}

func failPipeline(err error) int {
	code := clock.CodeOf(err)
	rep := errorReport{ErrorCode: string(code), Recoverable: false, Detail: err.Error()}
	switch code {
	case clock.CodeNegativeDuration, clock.CodeManifestInvalid, clock.CodeInputQuality,
		clock.CodeTruncatedSegment, clock.CodeTimescaleInvalid, clock.CodeTickOverflow,
		clock.CodeCodecUnsupported, clock.CodePathEscape, clock.CodeManifestTooLarge:
		return fail(rep, exitInputQuality)
	default:
		if code == "" {
			rep.ErrorCode = string(clock.CodeUnrecoverable)
		}
		return fail(rep, exitUnrecoverable)
	}
}

func emit(v any) {
	data, _ := json.MarshalIndent(v, "", "  ")
	fmt.Println(string(data))
}

func fail(rep errorReport, code int) int {
	emit(rep)
	return code
}
