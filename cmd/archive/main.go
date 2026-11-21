// Command archive is the terminal adapter for the Matroska/WebM archival
// pipeline. It exposes demo, verify and recover subcommands and maps the
// pipeline outcome to the documented process exit codes (0/2/1).
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"mkvarchive/internal/fixture"
	"mkvarchive/internal/pipeline"
	"mkvarchive/internal/timeline"
)

const usage = `usage:
  archive demo [--case standard|truncated|unknown-block|channels|samplerate|color] --out DIR
  archive verify --input ARCHIVE.mkv --manifest MANIFEST.json [--source SOURCE.mkv]
  archive recover --input DAMAGED.mkv --out DIR
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(1)
	}
	var err error
	switch os.Args[1] {
	case "demo":
		err = runDemo(os.Args[2:])
	case "verify":
		err = runVerify(os.Args[2:])
	case "recover":
		err = runRecover(os.Args[2:])
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(1)
	}
	if err != nil {
		if ae, ok := err.(*cliError); ok {
			fmt.Fprintln(os.Stderr, "error:", ae.msg)
			os.Exit(ae.code)
		}
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

type cliError struct {
	msg  string
	code int
}

func (e *cliError) Error() string { return e.msg }

func parseFlags(args []string) map[string]string {
	flags := map[string]string{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if len(a) > 2 && a[:2] == "--" {
			key := a[2:]
			if i+1 < len(args) && (len(args[i+1]) < 2 || args[i+1][:2] != "--") {
				flags[key] = args[i+1]
				i++
			} else {
				flags[key] = "true"
			}
		}
	}
	return flags
}

func runDemo(args []string) error {
	flags := parseFlags(args)
	outDir := flags["out"]
	if outDir == "" {
		return &cliError{"demo requires --out DIR", 1}
	}
	caseName := flags["case"]
	if caseName == "" {
		caseName = string(fixture.CaseStandard)
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	sample := fixture.Generate(fixture.Case(caseName), fixture.DemoClusters)
	sourcePath := filepath.Join(outDir, "sample.mkv")
	if err := os.WriteFile(sourcePath, sample.Data, 0o644); err != nil {
		return err
	}
	archivePath := filepath.Join(outDir, "archive.mkv")
	af, err := os.Create(archivePath)
	if err != nil {
		return err
	}
	defer af.Close()
	res, perr := pipeline.Archive(byteReader(sample.Data), af, outDir, pipeline.DefaultOptions())
	if perr != nil {
		return perr
	}
	if err = writeManifest(outDir, res.Manifest); err != nil {
		return err
	}
	if err = writePlan(outDir, res.PlanJSON); err != nil {
		return err
	}
	if err = writeQualityEvidence(outDir, res); err != nil {
		return err
	}
	printDemoFields(res)
	if res.Outcome == pipeline.OutcomeUnrecoverable {
		return &cliError{"input unrecoverable", 1}
	}
	if res.Outcome == pipeline.OutcomePartial {
		return &cliError{"partial recovery", 2}
	}
	return nil
}

func runRecover(args []string) error {
	flags := parseFlags(args)
	inPath := flags["input"]
	outDir := flags["out"]
	if inPath == "" || outDir == "" {
		return &cliError{"recover requires --input and --out", 1}
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	raw, err := os.ReadFile(inPath)
	if err != nil {
		return err
	}
	archivePath := filepath.Join(outDir, "recovered.mkv")
	af, err := os.Create(archivePath)
	if err != nil {
		return err
	}
	defer af.Close()
	res, perr := pipeline.Archive(byteReader(raw), af, outDir, pipeline.DefaultOptions())
	if perr != nil {
		return perr
	}
	if err = writeManifest(outDir, res.Manifest); err != nil {
		return err
	}
	printRecoverFields(res)
	switch res.Outcome {
	case pipeline.OutcomeSuccess:
		return nil
	case pipeline.OutcomePartial:
		return &cliError{"partial recovery", 2}
	default:
		return &cliError{"input unrecoverable", 1}
	}
}

func runVerify(args []string) error {
	flags := parseFlags(args)
	archivePath := flags["input"]
	manifestPath := flags["manifest"]
	if archivePath == "" || manifestPath == "" {
		return &cliError{"verify requires --input and --manifest", 1}
	}
	manifest, err := readManifest(manifestPath)
	if err != nil {
		return err
	}
	archiveBytes, err := os.ReadFile(archivePath)
	if err != nil {
		return err
	}
	sourcePath := flags["source"]
	if sourcePath == "" {
		// Default: the demo emits sample.mkv next to the archive.
		alt := filepath.Join(filepath.Dir(archivePath), "sample.mkv")
		if _, statErr := os.Stat(alt); statErr == nil {
			sourcePath = alt
		} else {
			sourcePath = archivePath
		}
	}
	sourceBytes, err := os.ReadFile(sourcePath)
	if err != nil {
		return err
	}
	// Duration derives from the manifest's widest track range.
	durationNS := manifestDurationNS(manifest)
	vr, err := pipeline.Verify(byteReader(sourceBytes), byteReader(archiveBytes),
		manifest, 12_000, durationNS)
	if err != nil {
		return err
	}
	printVerifyFields(vr)
	if !vr.PayloadSHA256Match || vr.ManifestAlignmentMS > 1 ||
		vr.BitrateDeviationPct > 1.0 {
		return &cliError{"verification thresholds exceeded", 1}
	}
	return nil
}

func manifestDurationNS(m *timeline.Manifest) int64 {
	var end int64
	for _, tr := range m.Tracks {
		if tr.EndNS > end {
			end = tr.EndNS
		}
	}
	// Include the final frame's cluster step (100 ms in the fixtures).
	return end + 100_000_000
}

func writeManifest(dir string, m *timeline.Manifest) error {
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "manifest.json"), raw, 0o644)
}

func writePlan(dir string, planJSON []byte) error {
	return os.WriteFile(filepath.Join(dir, "plan.json"), planJSON, 0o644)
}

func readManifest(path string) (*timeline.Manifest, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m timeline.Manifest
	if err = json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	return &m, nil
}
