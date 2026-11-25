// Command mediad runs the local resilient media processing pipeline.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"mediad/internal/fault"
	"mediad/internal/fixture"
	"mediad/internal/recovery"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: mediad <demo|verify|resume> [flags]")
		return 2
	}
	switch args[0] {
	case "demo":
		return runDemo(args[1:])
	case "verify":
		return runVerify(args[1:])
	case "resume":
		return runResume(args[1:])
	case "-h", "--help", "help":
		fmt.Println("usage: mediad <demo|verify|resume> [flags]")
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", args[0])
		return 2
	}
}

func runDemo(args []string) int {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	out := fs.String("out", "", "output root")
	seed := fs.Int64("seed", 41, "deterministic fixture seed")
	rate := fs.Int("rate", 48000, "target audio sample rate")
	layout := fs.String("layout", "stereo", "output layout (stereo)")
	lufs := fs.Float64("lufs", -18, "integrated loudness target")
	damage := fs.String("damage", "", "fixture damage: corrupt-mdat|truncated-mdat|truncated-box|missing-init|wrap-ts")
	interrupt := fs.String("interrupt-after", "", "durable stage after which to exit 75")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *out == "" {
		return argError("--out is required")
	}
	if *rate != 48000 {
		return argError("adapter v1 master rate must be 48000")
	}
	if *layout != "stereo" {
		return argError("adapter v1 supports --layout stereo")
	}
	if *lufs > 0 || *lufs < -70 {
		return argError("--lufs must be within [-70, 0]")
	}
	if *damage != "" && !knownDamage(*damage) {
		return argError("unknown --damage %q", *damage)
	}
	if *interrupt != "" && !knownStage(*interrupt) {
		return argError("unknown --interrupt-after stage %q", *interrupt)
	}

	if _, err := fixture.Generate(fixture.Options{Root: *out, Seed: *seed, Damage: *damage}); err != nil {
		return printError(err)
	}
	params := recovery.Params{
		Seed: *seed, Rate: *rate, Layout: *layout, LUFS: *lufs,
		SourceRate: fixture.SourceRate, SourceChannels: fixture.SourceChannels,
		Damage: *damage, Interrupt: *interrupt,
	}
	sum, err := recovery.Run(recovery.RunOptions{Root: *out, Params: params})
	if err != nil {
		if fault.IsInterrupted(err) {
			fmt.Printf("interrupted_after=%s\n", *interrupt)
			fmt.Println("resume_hint=run `mediad resume --root " + *out + "`")
		}
		return printError(err)
	}
	verified, err := recovery.Verify(*out)
	if err != nil {
		return printError(err)
	}
	printSummary(verified)
	_ = sum
	return 0
}

func runVerify(args []string) int {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	root := fs.String("root", "", "pipeline output root")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *root == "" {
		return argError("--root is required")
	}
	sum, err := recovery.Verify(*root)
	if err != nil {
		return printError(err)
	}
	printSummary(sum)
	fmt.Println("verify_ok=1")
	return 0
}

func runResume(args []string) int {
	fs := flag.NewFlagSet("resume", flag.ContinueOnError)
	root := fs.String("root", "", "interrupted pipeline root")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *root == "" {
		return argError("--root is required")
	}
	sum, err := recovery.Resume(*root)
	if err != nil {
		return printError(err)
	}
	printSummary(sum)
	fmt.Println("resume_match=1")
	return 0
}

func printSummary(s *recovery.Summary) {
	fmt.Println("mediad_status=ok")
	fmt.Printf("roundtrip=%.3f (%d/%d)\n", s.RoundtripRate, s.RoundtripMatched, s.RoundtripTotal)
	fmt.Printf("keyframe_hit=%.3f (%d/%d)\n", s.KeyframeHit, s.KeyframeMatched, s.KeyframeTotal)
	fmt.Printf("segment_sha256=%d\n", s.SegmentSHA256)
	fmt.Printf("replay_match=%d\n", boolInt(s.ReplayMatch))
	fmt.Printf("resume_match=%d\n", boolInt(s.ResumeMatch))
	fmt.Printf("output_sr=%d\n", s.OutputSampleRate)
	fmt.Printf("channels=%d\n", s.Channels)
	fmt.Printf("integrated_lufs=%.1f\n", s.IntegratedLUFS)
	fmt.Printf("true_peak_dbtp=%.2f\n", s.TruePeakDBTP)
	fmt.Printf("rtp_wrapped=%d\n", boolInt(s.RTPWrapped))
	fmt.Printf("periods=%d\n", s.Periods)
}

func printError(err error) int {
	var f *fault.Fault
	if errors.As(err, &f) {
		fmt.Printf("error_code=%s\n", f.Code)
		if f.Path != "" {
			fmt.Printf("path=%s\n", f.Path)
		}
		if f.BoxOffset >= 0 {
			fmt.Printf("box_offset=%d\n", f.BoxOffset)
			fmt.Println("first_box_offset=0")
		}
		if f.Offset >= 0 {
			fmt.Printf("offset=%d\n", f.Offset)
		}
		if f.Stage != "" {
			fmt.Printf("stage=%s\n", f.Stage)
		}
		if f.Err != nil {
			fmt.Fprintf(os.Stderr, "error=%v\n", f.Err)
		}
	} else {
		fmt.Printf("error_code=E_UNKNOWN\n")
		fmt.Fprintf(os.Stderr, "error=%v\n", err)
	}
	return fault.ExitCode(err)
}

func argError(format string, args ...any) int {
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintln(os.Stderr, "error_code=E_INVALID_ARGUMENT")
	fmt.Fprintln(os.Stderr, "error="+msg)
	return 2
}

func knownDamage(v string) bool {
	for _, d := range []string{
		fixture.DamageCorruptMDAT, fixture.DamageTruncatedMDAT,
		fixture.DamageTruncatedBox, fixture.DamageMissingInit, fixture.DamageWrapTS,
	} {
		if v == d {
			return true
		}
	}
	return false
}

func knownStage(v string) bool {
	for _, s := range recovery.StageOrder {
		if s == v {
			return true
		}
	}
	return false
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
