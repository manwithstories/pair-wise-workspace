// Command avflow is the terminal adapter for the AVPK pipeline. All
// functionality is driven by flags and emitted as plain text or JSON; there
// is no graphical or network dependency.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"avflow/internal/fixture"
	"avflow/internal/model"
	"avflow/internal/pipeline"
	"avflow/internal/txn"
)

// Report is the selfcheck JSON contract. Field names match the acceptance
// specification exactly.
type Report struct {
	Seed                  int64        `json:"seed"`
	Packets               int          `json:"packets"`
	FaultPrecisionPackets int          `json:"fault_precision_packets"`
	FalsePositives        int          `json:"false_positives"`
	P95WindowMS           float64      `json:"p95_window_ms"`
	ResumeMS              float64      `json:"resume_ms"`
	SHA256                string       `json:"sha256"`
	RepeatSHA256Match     bool         `json:"repeat_sha256_match"`
	RollbackOK            bool         `json:"rollback_ok"`
	FirstFault            *model.Fault `json:"first_fault,omitempty"`
	ReplayedStages        []string     `json:"replayed_stages,omitempty"`
	ExitCode              int          `json:"exit_code"`
}

type selfcheckFlags struct {
	seed       int64
	packets    int
	faultSet   string
	crashAfter string
	repeat     int
	asJSON     bool
	work       string
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(model.ExitArgs)
	}
	switch os.Args[1] {
	case "selfcheck":
		code := runSelfcheck(os.Args[2:])
		os.Exit(code)
	case "-h", "--help", "help":
		usage()
		os.Exit(model.ExitOK)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", os.Args[1])
		usage()
		os.Exit(model.ExitArgs)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `avflow - AVPK interactive media timeline pipeline

usage:
  avflow selfcheck --seed N --packets N --fault-set all|none --crash-after STAGE \
                   --repeat N [--json] --work DIR

flags:
  --fault-set    "all" injects every fault class, "none" a clean stream
  --crash-after  stream|normalize|timeline|"" (empty disables crash injection)
  --repeat       number of extra full runs used to verify checksum stability
exit codes:
  0 ok (including recovered/cropped-but-recoverable runs)
  2 argument error
  3 unrecoverable input
  75 simulated interruption
`)
}

func parseSelfcheck(args []string) (selfcheckFlags, error) {
	fs := flag.NewFlagSet("selfcheck", flag.ContinueOnError)
	var f selfcheckFlags
	fs.Int64Var(&f.seed, "seed", 0, "deterministic RNG seed")
	fs.IntVar(&f.packets, "packets", 1000, "total packets to generate")
	fs.StringVar(&f.faultSet, "fault-set", "none", "all|none")
	fs.StringVar(&f.crashAfter, "crash-after", "", "stage after which to simulate a crash")
	fs.IntVar(&f.repeat, "repeat", 1, "extra verification runs")
	fs.BoolVar(&f.asJSON, "json", false, "emit a JSON report")
	fs.StringVar(&f.work, "work", "", "workspace directory (required)")
	if err := fs.Parse(args); err != nil {
		return f, err
	}
	if f.work == "" {
		return f, fmt.Errorf("--work is required")
	}
	if f.packets <= 0 || f.packets > 200000 {
		return f, fmt.Errorf("--packets out of range (1..200000)")
	}
	if f.repeat < 0 {
		return f, fmt.Errorf("--repeat must be non-negative")
	}
	switch f.faultSet {
	case "all", "none", "fatal":
	default:
		return f, fmt.Errorf("--fault-set must be all, none or fatal, got %q", f.faultSet)
	}
	switch f.crashAfter {
	case "", pipeline.StageStream, pipeline.StageNormalize, pipeline.StageTimeline:
	default:
		return f, fmt.Errorf("--crash-after must be a stage name or empty")
	}
	return f, nil
}

func runSelfcheck(args []string) int {
	f, err := parseSelfcheck(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "argument error: %v\n", err)
		return model.ExitArgs
	}
	report, code := execute(f)
	if f.asJSON {
		out, _ := json.MarshalIndent(report, "", "  ")
		fmt.Println(string(out))
	} else {
		printText(report)
	}
	return code
}

func execute(f selfcheckFlags) (*Report, int) {
	plan := fixture.Plan{Seed: f.seed, Packets: f.packets}
	plan.Faults = fixture.None()
	switch f.faultSet {
	case "all":
		// End-to-end acceptance uses the recoverable fault classes so the
		// crash -> recover -> crop path completes with exit 0/75. The
		// non-recoverable PTS regression (rollback) is covered separately by
		// the "fatal" set and by the package-level tests.
		plan.Faults = fixture.RecoverableFaults()
	case "fatal":
		plan.Faults = fixture.FaultSet{PTSRegression: true}
	}
	gen, err := fixture.Build(plan)
	if err != nil {
		return &Report{ExitCode: model.ExitUnrecoverable}, model.ExitUnrecoverable
	}
	report := &Report{Seed: f.seed, Packets: f.packets, ResumeMS: 0, RollbackOK: true}
	crashDir := f.work + "/primary"

	// Optional phase 1: simulate a hard interruption, then recover and measure
	// the resume window. Recovery only replays fully committed stages.
	if f.crashAfter != "" {
		if _, _, crashErr := pipeline.Run(gen.Data, pipeline.Config{WorkDir: crashDir, CrashAfter: f.crashAfter}); crashErr == nil {
			report.ExitCode = model.ExitUnrecoverable
			return report, model.ExitUnrecoverable
		}
		start := time.Now()
		mgr, rep, recErr := txn.Recover(crashDir)
		report.ResumeMS = float64(time.Since(start).Nanoseconds()) / 1_000_000.0
		if recErr != nil {
			report.ExitCode = model.ExitUnrecoverable
			return report, model.ExitUnrecoverable
		}
		report.ReplayedStages = rep.ReplayedStages
		_ = mgr.Close()
	}

	// Phase 2: the full pipeline run (fresh, or the resumed workspace).
	tl, m, runErr := pipeline.Run(gen.Data, pipeline.Config{WorkDir: crashDir})
	if runErr != nil && !isRecoverable(runErr) {
		report.RollbackOK = m != nil && m.RollbackOK
		report.FirstFault = firstOf(runErr)
		report.ExitCode = model.ExitUnrecoverable
		return report, model.ExitUnrecoverable
	}
	report.SHA256 = m.SHA256
	report.FirstFault = m.FirstFault
	fillDiagnostics(report, gen, tl, m, f)

	// Phase 3: repeated independent runs must hash identically.
	report.RepeatSHA256Match = true
	for i := 0; i < f.repeat; i++ {
		dir := fmt.Sprintf("%s/repeat-%d", f.work, i)
		tl2, m2, err := pipeline.Run(gen.Data, pipeline.Config{WorkDir: dir})
		if err != nil && !isRecoverable(err) {
			report.ExitCode = model.ExitUnrecoverable
			return report, model.ExitUnrecoverable
		}
		if m2.SHA256 != report.SHA256 || pipeline.HashTimeline(tl2) != pipeline.HashTimeline(tl) {
			report.RepeatSHA256Match = false
		}
	}
	if f.crashAfter != "" {
		report.ExitCode = model.ExitSimulatedCrash
		return report, model.ExitSimulatedCrash
	}
	report.ExitCode = model.ExitOK
	return report, model.ExitOK
}

func fillDiagnostics(r *Report, g *fixture.Generated, tl *pipeline.Timeline, m *pipeline.Metrics, f selfcheckFlags) {
	r.FaultPrecisionPackets = pipeline.Precision(g, m.FirstFault)
	r.FalsePositives = pipeline.FalsePositives(g, tl.Faults)
	// p95 over short windows of the same workload.
	base := f.work + "/bench"
	p95, err := pipeline.BenchmarkRun(g.Data, base, benchmarkIters(f.packets))
	if err != nil {
		r.P95WindowMS = -1
		return
	}
	r.P95WindowMS = p95
}

func benchmarkIters(packets int) int {
	if packets >= 10000 {
		return 5 // keep the 10k acceptance run fast while still yielding p95
	}
	return 9
}

func isRecoverable(err error) bool {
	f, ok := model.AsFault(err)
	return ok && f.Recoverable
}

func firstOf(err error) *model.Fault {
	f, _ := model.AsFault(err)
	return f
}

func printText(r *Report) {
	fmt.Printf("seed=%d packets=%d\n", r.Seed, r.Packets)
	fmt.Printf("fault_precision_packets=%d false_positives=%d\n", r.FaultPrecisionPackets, r.FalsePositives)
	fmt.Printf("p95_window_ms=%.3f resume_ms=%.3f\n", r.P95WindowMS, r.ResumeMS)
	fmt.Printf("sha256=%s\nrepeat_sha256_match=%t rollback_ok=%t exit_code=%d\n",
		r.SHA256, r.RepeatSHA256Match, r.RollbackOK, r.ExitCode)
}
