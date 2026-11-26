// Command mediad is the pure-local terminal interface for media repair demos.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"mediad/internal/media"
	"mediad/internal/pipeline"
	"mediad/internal/verify"
)

func main() {
	root := &cobra.Command{
		Use:           "mediad",
		Short:         "Low-latency MP4/MOV timeline repair and recovery verifier",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(newSelfTestCmd(), newVerifyCmd())
	if err := root.Execute(); err != nil {
		var cli *pipeline.CLIError
		if errors.As(err, &cli) {
			fmt.Fprintf(os.Stderr, "mediad: %v\n", cli.Err)
			os.Exit(cli.Code)
		}
		fmt.Fprintf(os.Stderr, "mediad: internal error: %v\n", err)
		os.Exit(1)
	}
}

func newSelfTestCmd() *cobra.Command {
	var seed int64
	var duration time.Duration
	var loss float64
	var burst int
	var out string
	cmd := &cobra.Command{
		Use:   "selftest",
		Short: "Generate deterministic fixtures, repair them, and verify recovery",
		RunE: func(cmd *cobra.Command, args []string) error {
			report, err := pipeline.RunSelfTest(pipeline.SelfTestOptions{
				Seed: seed, Duration: duration, Loss: loss, Burst: burst, Out: out,
			})
			if report != nil {
				printSelfTest(report)
			}
			if err != nil {
				return err
			}
			if failures := report.Metrics.Failures(); len(failures) > 0 {
				return &pipeline.CLIError{Code: 3, Err: fmt.Errorf("metrics below acceptance thresholds: %v", failures)}
			}
			fmt.Println("exit_status=0 all_thresholds_passed")
			return nil
		},
	}
	cmd.Flags().Int64Var(&seed, "seed", 42, "deterministic PRNG seed")
	cmd.Flags().DurationVar(&duration, "duration", 60*time.Second, "generated media timeline duration")
	cmd.Flags().Float64Var(&loss, "loss", 0.03, "independent packet loss probability")
	cmd.Flags().IntVar(&burst, "burst", 7, "burst loss length in packets")
	cmd.Flags().StringVar(&out, "out", "mediad-run", "output directory")
	return cmd
}

func newVerifyCmd() *cobra.Command {
	var input string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "verify",
		Short: "Independently verify repaired MP4/MOV boundaries and index",
		RunE: func(cmd *cobra.Command, args []string) error {
			rep, err := verify.VerifyFile(input)
			if err != nil {
				f := media.AsFaultOrInternal(err)
				if f.Code == media.CodeInternal {
					return &pipeline.CLIError{Code: 1, Err: err}
				}
				return &pipeline.CLIError{Code: 2, Err: err}
			}
			if asJSON {
				data, _ := json.MarshalIndent(rep, "", "  ")
				fmt.Println(string(data))
			} else {
				printVerify(rep)
			}
			if !rep.OK {
				return &pipeline.CLIError{Code: 3, Err: fmt.Errorf("verification found %d boundary mismatches and %d seek misses", rep.BoundaryMismatch, rep.IndexSeekMiss)}
			}
			fmt.Println("exit_status=0 verification_passed")
			return nil
		},
	}
	cmd.Flags().StringVar(&input, "input", "", "local MP4/MOV file to verify")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit full JSON verification report")
	_ = cmd.MarkFlagRequired("input")
	return cmd
}

func printSelfTest(r *pipeline.SelfTestReport) {
	fmt.Println("mediad_selftest=1")
	fmt.Printf("seed=%d duration=%s loss=%.4f burst=%d out=%s\n", r.Seed, r.Duration, r.Loss, r.Burst, r.Out)
	printSummarySection("normal_samples", r.Normal)
	printSummarySection("damaged_samples", r.Damaged)
	printSummarySection("recovered_samples", r.Recovered)
	fmt.Println("metrics:")
	for _, line := range r.Metrics.KeyValueLines() {
		fmt.Println(line)
	}
	fmt.Printf("report_json=%s\n", r.Out+"/selftest-report.json")
}

func printSummarySection(title string, ss []pipeline.SampleSummary) {
	fmt.Printf("[%s] count=%d\n", title, len(ss))
	for _, s := range ss {
		fmt.Printf("- %s category=%s tracks=%d faults=%d fixes(ts=%d offset=%d edit=%d) loss=%d unrecovered=%d retrans=%d recovery_pct=%.2f seek_miss=%d boundary_mismatch=%d recoverable=%v\n",
			s.Name, s.Category, s.Tracks, s.FaultCount, s.TimestampFixes, s.OffsetFixes, s.EditFixes,
			s.LostPackets, s.Unrecovered, s.Retransmissions, s.RecoveryPct, s.IndexSeekMiss, s.BoundaryMismatch, s.Recoverable)
	}
}

func printVerify(r *verify.Report) {
	fmt.Println("mediad_verify=1")
	fmt.Printf("input=%s\n", r.Input)
	fmt.Printf("file_size=%d format=%s boxes=%d tracks=%d\n", r.FileSize, r.Format, r.BoxCount, len(r.Tracks))
	for _, t := range r.Tracks {
		fmt.Printf("track=%d kind=%s samples=%d sync=%d index_points=%d verified=%d ordered=%v offsets_in_bounds=%v\n",
			t.TrackID, t.Kind, t.Samples, t.SyncSamples, t.IndexPoints, t.VerifiedPoints, t.TimelineOrdered, t.OffsetsInBounds)
	}
	fmt.Printf("sync_max_ms=%.3f\n", r.SyncMaxMS)
	fmt.Printf("index_seek_miss=%d\n", r.IndexSeekMiss)
	fmt.Printf("boundary_mismatch=%d\n", r.BoundaryMismatch)
	fmt.Printf("ladder_mismatch=%d\n", r.LadderMismatch)
	fmt.Printf("output_sha256=%s\n", r.OutputSHA256)
	fmt.Printf("verify_ok=%v\n", r.OK)
}
