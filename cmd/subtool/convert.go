package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"subtool/internal/report"
	"subtool/internal/subtitle"
	"subtool/internal/timebase"
	"subtool/internal/verify"
)

func runConvert(args []string, stdout, stderr *os.File) int {
	flags := newFlagSet("convert", stderr)
	in := flags.String("in", "", "input subtitle")
	out := flags.String("out", "", "output subtitle")
	inFPS := flags.String("in-fps", "", "input frame rate")
	outFPS := flags.String("out-fps", "", "output frame rate")
	speedFollow := flags.Bool("speed-follow", false, "scale the timeline by the fps ratio")
	jsonPath := flags.String("json", "", "JSON report path")
	if err := flags.Parse(args); err != nil {
		return statusArgs
	}
	if *in == "" || *out == "" || *inFPS == "" || *outFPS == "" {
		fmt.Fprintln(stderr, "--in, --out, --in-fps and --out-fps are required")
		return statusArgs
	}
	target, err := formatFromPath(*out)
	if err != nil {
		return emitFailure(stderr, *jsonPath, &failure{code: "unsupported_format", message: err.Error(), status: statusArgs})
	}
	inRate, outRate, fail := parseRates(*inFPS, *outFPS)
	if fail != nil {
		return emitFailure(stderr, *jsonPath, fail)
	}
	_, enc, fail := loadSource(*in)
	if fail != nil {
		return emitFailure(stderr, *jsonPath, fail)
	}
	source, fail := parseDecoded(*in, enc)
	if fail != nil {
		return emitFailure(stderr, *jsonPath, fail)
	}
	enc.Text = nil
	resampled := source
	if *speedFollow {
		var err error
		resampled, err = timebase.Resample(source, inRate, outRate, true)
		if err != nil {
			return emitFailure(stderr, *jsonPath, &failure{code: "resample_failed", message: err.Error(), status: statusInput})
		}
	}
	subtitle.PrepareForFormat(resampled, target)
	data, err := subtitle.Write(resampled, target)
	if err != nil {
		return emitFailure(stderr, *jsonPath, &failure{code: "write_failed", message: err.Error(), status: statusInput})
	}
	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		return emitFailure(stderr, *jsonPath, &failure{code: "write_failed", message: err.Error(), status: statusInput})
	}
	if err := os.WriteFile(*out, data, 0o644); err != nil {
		return emitFailure(stderr, *jsonPath, &failure{code: "write_failed", message: err.Error(), status: statusInput})
	}
	drops := report.Drops(resampled)
	value := report.ConvertReport{
		Input: *in, Output: *out, InFPS: inRate.String(), OutFPS: outRate.String(),
		Mode: string(timebase.ModeFor(*speedFollow)), Encoding: enc, CuesTotal: len(source.Cues),
		Dropped: drops, DroppedTotal: len(drops), QuantizedMaxMS: quantizedMax(resampled, target),
		Warnings: lowConfidenceWarnings(enc),
	}
	if err := report.WriteJSON(*jsonPath, value); *jsonPath != "" && err != nil {
		return emitFailure(stderr, "", &failure{code: "report_write_failed", message: err.Error(), status: statusInput})
	}
	report.PrintConvert(stdout, value)
	return encodingStatus(enc, false)
}

func runRoundtrip(args []string, stdout, stderr *os.File) int {
	started := time.Now()
	flags := newFlagSet("roundtrip", stderr)
	in := flags.String("in", "", "input subtitle")
	via := flags.String("via", "", "intermediate format srt|ass|vtt")
	inFPS := flags.String("in-fps", "", "input frame rate")
	outFPS := flags.String("out-fps", "", "output frame rate")
	tolerance := flags.Int64("tolerance-ms", 0, "maximum accepted drift in milliseconds")
	speedFollow := flags.Bool("speed-follow", false, "scale the timeline by the fps ratio")
	jsonPath := flags.String("json", "", "JSON report path")
	if err := flags.Parse(args); err != nil {
		return statusArgs
	}
	if *in == "" || *via == "" || *inFPS == "" || *outFPS == "" || *tolerance < 0 {
		fmt.Fprintln(stderr, "--in, --via, --in-fps, --out-fps and a non-negative --tolerance-ms are required")
		return statusArgs
	}
	viaFormat, err := parseFormat(*via)
	if err != nil {
		return emitFailure(stderr, *jsonPath, &failure{code: "unsupported_format", message: err.Error(), status: statusArgs})
	}
	inRate, outRate, fail := parseRates(*inFPS, *outFPS)
	if fail != nil {
		return emitFailure(stderr, *jsonPath, fail)
	}
	_, enc, fail := loadSource(*in)
	if fail != nil {
		return emitFailure(stderr, *jsonPath, fail)
	}
	source, fail := parseDecoded(*in, enc)
	if fail != nil {
		return emitFailure(stderr, *jsonPath, fail)
	}
	enc.Text = nil
	expected := source
	if *speedFollow {
		var err error
		expected, err = timebase.Resample(source, inRate, outRate, true)
		if err != nil {
			return emitFailure(stderr, *jsonPath, &failure{code: "resample_failed", message: err.Error(), status: statusInput})
		}
	}
	subtitle.PrepareForFormat(expected, viaFormat)
	data, err := subtitle.Write(expected, viaFormat)
	if err != nil {
		return emitFailure(stderr, *jsonPath, &failure{code: "write_failed", message: err.Error(), status: statusInput})
	}
	actual, err := parseFormatBytes(viaFormat, data)
	if err != nil {
		return emitFailure(stderr, *jsonPath, &failure{code: "roundtrip_reparse_failed", message: err.Error(), status: statusInput})
	}
	data = nil
	result := verify.Compare(expected, actual, inRate, outRate)
	elapsed := time.Since(started).Milliseconds()
	passed := result.Passed(*tolerance, viaFormat)
	value := report.RoundtripReport{
		Input: *in, Via: string(viaFormat), InFPS: inRate.String(), OutFPS: outRate.String(),
		Mode: string(timebase.ModeFor(*speedFollow)), ToleranceMS: *tolerance, Encoding: enc,
		ElapsedMS: elapsed, Passed: passed, QuantizedMaxMS: result.QuantizedMaxMS(viaFormat),
		Warnings: lowConfidenceWarnings(enc), Result: result,
	}
	if err := report.WriteJSON(*jsonPath, value); *jsonPath != "" && err != nil {
		return emitFailure(stderr, "", &failure{code: "report_write_failed", message: err.Error(), status: statusInput})
	}
	report.PrintRoundtrip(stdout, value)
	return encodingStatus(enc, !passed)
}

func parseFormatBytes(format subtitle.Format, data []byte) (*subtitle.Track, error) {
	switch format {
	case subtitle.FormatSRT:
		return subtitle.ParseSRT(data)
	case subtitle.FormatASS:
		return subtitle.ParseASS(data)
	case subtitle.FormatVTT:
		return subtitle.ParseVTT(data)
	default:
		return nil, fmt.Errorf("unsupported format %q", format)
	}
}

func quantizedMax(track *subtitle.Track, target subtitle.Format) int64 {
	if target != subtitle.FormatASS {
		return 0
	}
	var maximum int64
	for _, cue := range subtitle.Eligible(track.Cues) {
		maximum = max64(maximum, abs64(quantize10(cue.StartMS)-cue.StartMS))
		maximum = max64(maximum, abs64(quantize10(cue.EndMS)-cue.EndMS))
	}
	return maximum
}

func quantize10(ms int64) int64 {
	return ((ms + 5) / 10) * 10
}

func abs64(value int64) int64 {
	if value < 0 {
		return -value
	}
	return value
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
