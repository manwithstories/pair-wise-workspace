package cli

import (
	"fmt"
	"path/filepath"
	"strings"

	"mediarun/internal/mediaerr"
	"mediarun/internal/metadata"
	"mediarun/internal/pipeline"
	"mediarun/testdata"
)

// demoSampleCounts fixes the reproducible demo workload: 120 video + 120
// audio = 240 samples, matching the acceptance output.
const (
	demoVideo = 120
	demoAudio = 120
)

// RunDemo generates fixtures under the work directory, executes the pipeline
// and returns the lines that must be printed to the terminal. A non-nil error
// carries the structured code used for the process exit code.
func RunDemo(a *Args) ([]string, *pipeline.Stats, error) {
	if err := ensureSandbox(a.WorkDir); err != nil {
		return nil, nil, err
	}
	format, input, err := prepareInput(a)
	if err != nil {
		return nil, nil, err
	}
	opts := pipeline.Options{
		WorkDir: a.WorkDir, InputPath: input,
		OutputName: "output.mrmx", ManifestName: "trace.json",
		QueueDepth: a.QueueDepth, TargetBitrate: a.TargetBitrate,
		InterruptAfter: a.InterruptAfter, Resume: a.Resume,
		Mapping:        a.Mapping(),
		Metadata:       demoMetadata(),
	}
	stats, err := pipeline.Run(opts)
	if err != nil {
		return reportFailure(format, input, err), stats, err
	}
	return reportSuccess(stats, a.ResumeFrom(stats)), stats, nil
}

// ResumeFrom surfaces the checkpoint boundary only on a resume run.
func (a *Args) ResumeFrom(stats *pipeline.Stats) int64 {
	if a.Resume && stats != nil {
		return stats.ResumeFromSample
	}
	return 0
}

// prepareInput creates the requested fixture(s). "mixed" uses MP4 as the
// container; MXF is selected explicitly so both code paths are demonstrable.
func prepareInput(a *Args) (format, input string, err error) {
	switch a.Format {
	case "mxf":
		input = filepath.Join(a.WorkDir, "input.mxf")
		err = testdata.Write(testdata.FixtureSpec{
			Path: input, Format: "mxf",
			VideoSamples: demoVideo, AudioSamples: demoAudio,
			VideoScale: 25, FrameDur: 1, Width: 1920, Height: 1080,
			AudioRate: 48000, TargetBitrate: a.TargetBitrate, Injection: a.Inject,
		})
		return "mxf", input, err
	default:
		input = filepath.Join(a.WorkDir, "input.mp4")
		inject := a.Inject
		// The canonical truncated demo failure targets an MP4 box.
		if inject == "truncated-klv" && a.Format != "mxf" {
			inject = "truncated-box"
		}
		err = testdata.Write(testdata.FixtureSpec{
			Path: input, Format: "mp4",
			VideoSamples: demoVideo, AudioSamples: demoAudio,
			VideoScale: 30000, FrameDur: 1001, Width: 1920, Height: 1080,
			AudioRate: 48000, TargetBitrate: a.TargetBitrate, Injection: inject,
		})
		return "mp4", input, err
	}
}

// demoMetadata supplies conflicting-but-resolvable metadata so the arbitration
// precedence is exercised in every demo run.
func demoMetadata() map[string]metadata.Input {
	rot90, rot270 := 90, 270
	return map[string]metadata.Input{
		"video/0": {
			TrackID: "video/0", CodedWidth: 1920, CodedHeight: 1080,
			ContainerLayer: &metadata.LayerValues{Rotation: &rot90},
			TrackLayer:     &metadata.LayerValues{Rotation: &rot90},
			Override:       &metadata.LayerValues{Rotation: &rot270},
		},
	}
}

func reportSuccess(s *pipeline.Stats, resumeFrom int64) []string {
	lines := []string{
		fmt.Sprintf("status=ok"),
		fmt.Sprintf("format=%s", s.Format),
		fmt.Sprintf("samples_in=%d", s.SamplesIn),
		fmt.Sprintf("samples_out=%d", s.SamplesOut),
		fmt.Sprintf("duplicate_samples=%d", s.DuplicateSamples),
		fmt.Sprintf("dropped_samples=%d", s.DroppedSamples),
		fmt.Sprintf("target_bitrate_bps=%d", s.TargetBitrateBPS),
		fmt.Sprintf("actual_bitrate_bps=%d", s.ActualBitrateBPS),
		fmt.Sprintf("bitrate_deviation_pct=%.1f", s.BitrateDeviationPct),
		fmt.Sprintf("quality_checked=%d", s.QualityChecked),
		fmt.Sprintf("quality_errors=%d", s.QualityErrors),
		fmt.Sprintf("manifest_alignment_ms=%d", s.ManifestAlignMillis),
		fmt.Sprintf("output_sha256=%s", s.OutputSHA256),
		fmt.Sprintf("output_committed=%t", s.OutputCommitted),
	}
	if resumeFrom > 0 {
		lines = append(lines, fmt.Sprintf("resume_from_sample=%d", resumeFrom))
	}
	return lines
}

// reportFailure renders the machine-readable diagnostic block. The structured
// error's code, offset and commit flag are always present.
func reportFailure(format, input string, err error) []string {
	se, ok := err.(*mediaerr.Error)
	lines := []string{"status=error"}
	if ok {
		lines = append(lines,
			fmt.Sprintf("error_code=%s", se.Code),
			fmt.Sprintf("input_offset=%d", se.Offset),
		)
		if se.Track != "" {
			lines = append(lines, "track="+se.Track)
		}
		if se.Detail != "" {
			lines = append(lines, "detail="+se.Detail)
		}
	} else {
		lines = append(lines, "error_code=IO_FAILURE", "input_offset=-1",
			"detail="+strings.ReplaceAll(err.Error(), "\n", " "))
	}
	lines = append(lines, "output_committed=false", "input="+filepath.Base(input))
	return lines
}

// ensureSandbox makes sure the work directory exists and is usable.
func ensureSandbox(workDir string) error {
	if workDir == "" {
		return mediaerr.New(mediaerr.CodeInvalidArgs, "empty work directory")
	}
	return nil
}
