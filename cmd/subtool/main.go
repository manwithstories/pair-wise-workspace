package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"

	"subtool/internal/encsniff"
	"subtool/internal/report"
	"subtool/internal/subtitle"
	"subtool/internal/timebase"
)

const (
	statusOK       = 0
	statusMismatch = 1
	statusLowEnc   = 2
	statusInput    = 3
	statusArgs     = 4
	maxInputBytes  = 200 * 1024 * 1024
)

type failure struct {
	code       string
	message    string
	parsedCues int
	status     int
}

type errorEnvelope struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	ParsedCues int    `json:"parsed_cues"`
}

func main() {
	debug.SetGCPercent(20)
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr *os.File) int {
	if len(args) == 0 {
		printUsage(stderr)
		return statusArgs
	}
	switch args[0] {
	case "fixtures":
		return runFixtures(args[1:], stdout, stderr)
	case "probe":
		return runProbe(args[1:], stdout, stderr)
	case "frame":
		return runFrame(args[1:], stdout, stderr)
	case "convert":
		return runConvert(args[1:], stdout, stderr)
	case "roundtrip":
		return runRoundtrip(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown subcommand %q\n", args[0])
		printUsage(stderr)
		return statusArgs
	}
}

func printUsage(out *os.File) {
	fmt.Fprintln(out, `usage:
  subtool fixtures gen --out DIR --count N --seed N
  subtool probe --in FILE [--json FILE]
  subtool frame --in FILE --fps NUM/DEN [--json FILE]
  subtool convert --in FILE --out FILE --in-fps RATE --out-fps RATE [--speed-follow] [--json FILE]
  subtool roundtrip --in FILE --via srt|ass|vtt --in-fps RATE --out-fps RATE [--tolerance-ms N] [--speed-follow] [--json FILE]`)
}

func newFlagSet(name string, stderr *os.File) *flag.FlagSet {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(stderr)
	return flags
}

func parseRates(inValue, outValue string) (timebase.Rate, timebase.Rate, *failure) {
	inRate, err := timebase.ParseRate(inValue)
	if err != nil {
		return timebase.Rate{}, timebase.Rate{}, &failure{code: "invalid_fps", message: err.Error(), status: statusArgs}
	}
	outRate, err := timebase.ParseRate(outValue)
	if err != nil {
		return timebase.Rate{}, timebase.Rate{}, &failure{code: "invalid_fps", message: err.Error(), status: statusArgs}
	}
	return inRate, outRate, nil
}

func parseFormat(value string) (subtitle.Format, error) {
	switch strings.ToLower(strings.TrimSpace(strings.TrimPrefix(value, "."))) {
	case "srt":
		return subtitle.FormatSRT, nil
	case "ass", "ssa":
		return subtitle.FormatASS, nil
	case "vtt":
		return subtitle.FormatVTT, nil
	default:
		return "", fmt.Errorf("unsupported subtitle format %q", value)
	}
}

func formatFromPath(path string) (subtitle.Format, error) {
	ext := filepath.Ext(path)
	if ext == "" {
		return "", fmt.Errorf("cannot infer output format from %q", path)
	}
	return parseFormat(ext)
}

func loadSource(path string) ([]byte, encsniff.Result, *failure) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, encsniff.Result{}, &failure{code: "input_unavailable", message: err.Error(), status: statusInput}
	}
	if info.Size() > maxInputBytes {
		return nil, encsniff.Result{}, &failure{code: "input_too_large", message: "input exceeds 200MB limit", status: statusInput}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, encsniff.Result{}, &failure{code: "input_unavailable", message: err.Error(), status: statusInput}
	}
	enc, err := encsniff.Detect(data)
	if err != nil {
		return nil, enc, &failure{code: "encoding_error", message: err.Error(), status: statusInput}
	}
	return data, enc, nil
}

func parseDecoded(path string, enc encsniff.Result) (*subtitle.Track, *failure) {
	format := subtitle.DetectFormat(path, enc.Text)
	var track *subtitle.Track
	var err error
	switch format {
	case subtitle.FormatSRT:
		track, err = subtitle.ParseSRT(enc.Text)
	case subtitle.FormatASS:
		track, err = subtitle.ParseASS(enc.Text)
	case subtitle.FormatVTT:
		track, err = subtitle.ParseVTT(enc.Text)
	default:
		return nil, &failure{code: "unsupported_format", message: "unsupported subtitle format", status: statusInput}
	}
	if err != nil {
		if parseErr, ok := err.(*subtitle.ParseError); ok {
			return track, &failure{code: parseErr.Code, message: parseErr.Message, parsedCues: parseErr.ParsedCues, status: statusInput}
		}
		return track, &failure{code: "parse_error", message: err.Error(), status: statusInput}
	}
	return track, nil
}

func lowConfidenceWarnings(enc encsniff.Result) []string {
	if enc.Low {
		return []string{"encoding_low_confidence"}
	}
	return nil
}

func writeFailureJSON(path string, fail *failure) {
	if path == "" {
		return
	}
	_ = report.WriteJSON(path, errorEnvelope{Error: errorBody{
		Code: fail.code, Message: fail.message, ParsedCues: fail.parsedCues,
	}})
}

func emitFailure(stderr *os.File, jsonPath string, fail *failure) int {
	writeFailureJSON(jsonPath, fail)
	fmt.Fprintf(stderr, "%s: %s (parsed_cues=%d)\n", fail.code, fail.message, fail.parsedCues)
	return fail.status
}

func encodingStatus(enc encsniff.Result, failed bool) int {
	if failed {
		return statusMismatch
	}
	if enc.Low {
		return statusLowEnc
	}
	return statusOK
}
