// Command mediad is the terminal entry point for the multi-camera media
// delivery pipeline: fixture generation, delivery with fault injection,
// crash-safe resume and verification.
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"mediad/internal/fixture"
	"mediad/internal/model"
	"mediad/internal/output"
	"mediad/internal/pipeline"
)

// report is the stable JSON contract emitted by deliver/resume/verify.
type report struct {
	InputSHA256        string `json:"input_sha256"`
	OutputSHA256       string `json:"output_sha256"`
	FirstBadOffset     int64  `json:"first_bad_offset"`
	RecoveredPackets   int    `json:"recovered_packets"`
	UnrecoveredPackets int    `json:"unrecovered_packets"`
	AVDriftMS          int64  `json:"av_drift_ms"`
	ResumeMS           int64  `json:"resume_ms"`
	ManifestPath       string `json:"manifest_path,omitempty"`
}

func main() {
	root := &cobra.Command{
		Use:           "mediad",
		Short:         "Multi-camera MP4/MOV delivery pipeline (scan, RTP recovery, sync, remux)",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(fixtureCmd(), deliverCmd(), resumeCmd(), verifyCmd())
	err := root.Execute()
	if err == nil {
		return
	}
	// ErrInterrupted is an expected control-flow outcome (exit 75), not a
	// user-facing failure, so nothing is printed to stderr.
	if err != model.ErrInterrupted {
		fmt.Fprintln(os.Stderr, "error:", err)
	}
	os.Exit(exitCodeFor(err))
}

func fixtureCmd() *cobra.Command {
	var dir string
	cmd := &cobra.Command{
		Use:   "fixture",
		Short: "Generate deterministic synthetic media and a loss profile",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := fixture.Generate(dir); err != nil {
				return err
			}
			fmt.Fprintf(os.Stdout, "fixtures written to %s\n", dir)
			return nil
		},
	}
	cmd.Flags().StringVar(&dir, "dir", "", "output directory for fixtures")
	_ = cmd.MarkFlagRequired("dir")
	return cmd
}

func deliverCmd() *cobra.Command {
	c := newRunConfig()
	cmd := &cobra.Command{
		Use:   "deliver",
		Short: "Run the delivery pipeline, optionally stopping early (exit 75)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return executeRun(c, false)
		},
	}
	bindRunFlags(cmd, c)
	return cmd
}

func resumeCmd() *cobra.Command {
	c := newRunConfig()
	cmd := &cobra.Command{
		Use:   "resume",
		Short: "Resume an interrupted delivery from its last valid checkpoint",
		RunE: func(cmd *cobra.Command, args []string) error {
			return executeRun(c, true)
		},
	}
	bindRunFlags(cmd, c)
	return cmd
}

func verifyCmd() *cobra.Command {
	var inputDir, out string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "verify",
		Short: "Verify a deliverable against its provenance manifest",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runVerify(inputDir, out, asJSON)
		},
	}
	cmd.Flags().StringVar(&inputDir, "input-dir", "", "source fixture directory")
	cmd.Flags().StringVar(&out, "out", "", "delivery output directory")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the report as JSON")
	_ = cmd.MarkFlagRequired("input-dir")
	_ = cmd.MarkFlagRequired("out")
	return cmd
}

type runConfig struct {
	inputDir         string
	out              string
	faultProfile     string
	stopAfterSamples int
}

func newRunConfig() *runConfig { return &runConfig{} }

func bindRunFlags(cmd *cobra.Command, c *runConfig) {
	cmd.Flags().StringVar(&c.inputDir, "input-dir", "", "source fixture directory")
	cmd.Flags().StringVar(&c.out, "out", "", "delivery output directory")
	cmd.Flags().StringVar(&c.faultProfile, "fault-profile", "", "JSON fault profile")
	cmd.Flags().IntVar(&c.stopAfterSamples, "stop-after-samples", 0,
		"interrupt after N samples (exit 75)")
	_ = cmd.MarkFlagRequired("input-dir")
	_ = cmd.MarkFlagRequired("out")
}

func executeRun(c *runConfig, resume bool) error {
	res, err := pipeline.Run(pipeline.Config{
		InputDir:         c.inputDir,
		OutDir:           c.out,
		FaultProfile:     c.faultProfile,
		StopAfterSamples: c.stopAfterSamples,
		Resume:           resume,
	})
	// A result may accompany a hard input fault (exit 2): the manifest was
	// still written, so emit its report before propagating the error.
	if res != nil {
		r := reportFromManifest(res)
		r.ResumeMS = res.ResumeMS
		emitJSON(r)
	}
	if err != nil {
		return err // ErrInterrupted -> 75, hard fault -> 2
	}
	if res == nil {
		return model.ErrVerification
	}
	return nil
}

func reportFromManifest(res *pipeline.Result) report {
	m := res.Manifest
	return report{
		InputSHA256:        m.InputSHA256,
		OutputSHA256:       m.OutputSHA256,
		FirstBadOffset:     m.FirstBadOffset,
		RecoveredPackets:   m.RecoveredPackets,
		UnrecoveredPackets: m.UnrecoveredPackets,
		AVDriftMS:          m.AVDriftMS,
		ManifestPath:       output.ManifestName,
	}
}

func runVerify(inputDir, out string, asJSON bool) error {
	manifest, err := output.LoadManifest(out)
	if err != nil {
		return fmt.Errorf("%w: %v", model.ErrVerification, err)
	}
	// recompute the input digest independently and compare
	entries, err := os.ReadDir(inputDir)
	if err != nil {
		return err
	}
	var inputPath string
	for _, e := range entries {
		if e.Name() == fixture.NameNormal {
			inputPath = inputDir + "/" + e.Name()
		}
	}
	if inputPath != "" {
		digest, err := indexDigest(inputPath)
		if err != nil {
			return err
		}
		if digest != manifest.InputSHA256 {
			return fmt.Errorf("%w: input digest changed", model.ErrVerification)
		}
	}
	r := report{
		InputSHA256:        manifest.InputSHA256,
		OutputSHA256:       manifest.OutputSHA256,
		FirstBadOffset:     manifest.FirstBadOffset,
		RecoveredPackets:   manifest.RecoveredPackets,
		UnrecoveredPackets: manifest.UnrecoveredPackets,
		AVDriftMS:          manifest.AVDriftMS,
		ManifestPath:       output.ManifestName,
	}
	if asJSON {
		emitJSON(r)
	} else {
		fmt.Printf("verified: output_sha256=%s recovered=%d unrecovered=%d drift_ms=%d\n",
			r.OutputSHA256, r.RecoveredPackets, r.UnrecoveredPackets, r.AVDriftMS)
	}
	return nil
}

func emitJSON(r report) {
	b, _ := json.MarshalIndent(r, "", "  ")
	fmt.Println(string(b))
}

// exitCodeFor maps domain errors onto the documented process exit codes.
func exitCodeFor(err error) int {
	if err == model.ErrInterrupted {
		return model.ExitInterrupted // 75
	}
	return model.ExitInputFailure // 2 for any input/verification failure
}
