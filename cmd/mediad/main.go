// Command mediad is the terminal entry point for the mediad processing
// library. It demonstrates the pipeline end to end and prints the acceptance
// fields; all real work lives in the internal packages.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"mediad/internal/fixture"
	"mediad/internal/model"
	"mediad/internal/normalize"
	"mediad/internal/probe"
)

// Exit codes are part of the command's contract.
const (
	exitOK        = 0
	exitFailure   = 1
	exitInvalid   = 2
	exitRecovery  = 3
	exitThreshold = 4
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return exitInvalid
	}
	switch args[0] {
	case "demo":
		return runDemo(args[1:], stdout, stderr)
	case "probe":
		return runProbe(args[1:], stdout, stderr)
	case "fixtures":
		return runFixtures(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		usage(stdout)
		return exitOK
	default:
		fmt.Fprintf(stderr, "mediad: unknown command %q\n", args[0])
		usage(stderr)
		return exitInvalid
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `mediad - deterministic media timeline verifier

usage:
  mediad demo  [flags]     run the full pipeline and print acceptance fields
  mediad probe <file>      print container metadata for a generated sample
  mediad fixtures <dir>    write the demo's media samples into <dir>
  mediad help              show this message

demo flags:
  --seed int               random seed for fixtures and payloads (default 17)
  --faults list            comma list of duplicate,reorder,burst-loss,corrupt-log
  --bandwidth-mbps list    comma list of bandwidth values in Mbps
  --interrupt-after stage  simulate a crash after this stage
  --resume                 reuse checksum-verified stages after a crash
  --dir path               working directory (default: temporary)
  --seeks int              index probes per track (default 11)
  --max-burst int          longest recoverable packet gap (default 8)
  --max-alignment-ms float alignment gate in milliseconds (default 1)
  --max-bitrate-delta-pct  bitrate deviation gate in percent (default 5)
  --min-psnr float         minimum quality gate in dB (default 30)
  --json                   print only the machine-readable summary

exit codes:
  0 verified   2 invalid input or metadata   3 recovery or commit failed
  4 quantitative gate failed
`)
}

func runProbe(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "mediad: probe takes exactly one file")
		return exitInvalid
	}
	path := args[0]
	var (
		c   model.Container
		err error
	)
	switch strings.ToLower(filepath.Ext(path)) {
	case ".mp4", ".m4v", ".mov":
		c, err = probe.MP4(path, probe.DefaultLimits())
	case ".mxf":
		c, err = probe.MXF(path)
	default:
		// Fall back to sniffing so extension-less samples still work.
		if c, err = probe.MXF(path); err != nil {
			c, err = probe.MP4(path, probe.DefaultLimits())
		}
	}
	if err != nil {
		fmt.Fprintf(stderr, "mediad: %v\n", err)
		code := model.ExitCodeOf(err)
		if code == 0 {
			code = exitFailure
		}
		return code
	}
	if _, err := normalize.Container(&c); err != nil {
		fmt.Fprintf(stderr, "mediad: %v\n", err)
		return model.ExitCodeOf(err)
	}
	// Samples are summarised rather than dumped: the probe is for inspecting
	// metadata, and a full sample table would bury it.
	type trackView struct {
		ID         uint32           `json:"id"`
		Kind       string           `json:"kind"`
		Codec      string           `json:"codec"`
		Timescale  uint32           `json:"timescale"`
		FrameRate  string           `json:"frame_rate"`
		Start      string           `json:"start"`
		Duration   string           `json:"duration"`
		Display    string           `json:"display"`
		Rotation   int              `json:"rotation"`
		Crop       model.Crop       `json:"crop"`
		Color      model.ColorSpace `json:"color"`
		Conflicts  int              `json:"color_conflicts"`
		ColorTrace int              `json:"color_candidates"`
		Samples    int              `json:"samples"`
		Keyframes  int              `json:"keyframes"`
	}
	// Normalisation has already run, so rotation is snapped and colour
	// arbitrated by the same code the pipeline uses.
	view := struct {
		Format string      `json:"format"`
		Tracks []trackView `json:"tracks"`
	}{Format: c.Format}
	for _, t := range c.Tracks {
		keys := 0
		for _, s := range t.Samples {
			if s.Keyframe {
				keys++
			}
		}
		view.Tracks = append(view.Tracks, trackView{
			ID: t.ID, Kind: string(t.Kind), Codec: t.Codec, Timescale: t.Timescale,
			FrameRate: t.FrameRate.String(), Start: t.Start.String(), Duration: t.Duration.String(),
			Display:  fmt.Sprintf("%dx%d", t.DisplayWidth(), t.DisplayHeight()),
			Rotation: t.Rotation, Crop: t.Crop, Color: t.Color.Chosen,
			Conflicts: t.Color.Conflicts, ColorTrace: len(t.Color.Trace),
			Samples: len(t.Samples), Keyframes: keys,
		})
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(view); err != nil {
		fmt.Fprintf(stderr, "mediad: %v\n", err)
		return exitFailure
	}
	return exitOK
}

func parseFaults(s string) ([]fixture.FaultKind, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	var out []fixture.FaultKind
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		k := fixture.FaultKind(part)
		switch k {
		case fixture.FaultDuplicate, fixture.FaultReorder, fixture.FaultBurstLoss, fixture.FaultCorruptLog:
			out = append(out, k)
		default:
			return nil, fmt.Errorf("unknown fault %q (want duplicate, reorder, burst-loss or corrupt-log)", part)
		}
	}
	return out, nil
}

func parseFloats(s string) ([]float64, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	var out []float64
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		v, err := strconv.ParseFloat(part, 64)
		if err != nil || v <= 0 {
			return nil, fmt.Errorf("%q is not a positive number", part)
		}
		out = append(out, v)
	}
	return out, nil
}
