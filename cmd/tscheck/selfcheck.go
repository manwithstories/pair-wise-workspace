package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"tscheck/internal/fanout"
	"tscheck/internal/fixture"
	"tscheck/internal/recovery"
	"tscheck/internal/ts"
)

// selfcheckReport is the JSON verification summary printed by selfcheck.
type selfcheckReport struct {
	DamageOffsetErrorBytes int64   `json:"damage_offset_error_bytes"`
	ResumeMs               float64 `json:"resume_ms"`
	BoundaryDeltaTicks     int64   `json:"boundary_delta_ticks"`
	SHA256                 string  `json:"sha256"`
	RepeatSHA256Equal      bool    `json:"repeat_sha256_equal"`
	Packets                int     `json:"packets"`
	KeyframeP50Ticks       int64   `json:"keyframe_interval_p50_ticks"`
	Drift                  float64 `json:"drift"`
	FaultsTotal            int     `json:"faults_total"`
	UnrecoverableSections  int     `json:"unrecoverable_sections"`
	BoundaryConflicts      int     `json:"boundary_conflicts"`
}

func cmdSelfcheck(args []string) int {
	fs := flag.NewFlagSet("selfcheck", flag.ContinueOnError)
	seed := fs.Int64("seed", 41, "deterministic seed")
	bytes := fs.Int("bytes", 2<<20, "approximate sample size in bytes")
	out := fs.String("out", "", "output directory (under /tmp)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *out == "" || *bytes < 10*ts.PacketSize {
		fmt.Fprintln(os.Stderr, "selfcheck: invalid arguments")
		return exitUsage
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "selfcheck:", err)
		return exitUsage
	}
	rep := selfcheckReport{}

	// Path 1: normal three-bitrate ladder with VFR, merged boundaries.
	var branches []fanout.Branch
	for i := 0; i < 3; i++ {
		data, _ := fixture.Generate(fixture.Options{
			Seed: *seed, Bytes: *bytes, Branch: i, VFR: true})
		branches = append(branches, fanout.Branch{
			Name: fmt.Sprintf("branch%d", i), Data: data})
	}
	results := fanout.Run(branches)
	merge := fanout.MergeBoundaries(results)
	rep.BoundaryDeltaTicks = int64(merge.MaxDelta)
	rep.BoundaryConflicts = len(merge.Conflicts)
	rep.Packets = results[0].Packets
	rep.KeyframeP50Ticks = int64(results[0].GOP.IntervalP50)
	rep.Drift = results[0].Drift
	for _, r := range results {
		rep.FaultsTotal += len(r.Faults)
		for _, f := range r.Faults {
			if !f.Kind.Recoverable() {
				rep.UnrecoverableSections++
			}
		}
	}

	// Path 2: damaged stream; the scanner must locate the first injected
	// fault within one packet.
	damaged, injected := fixture.Generate(fixture.Options{
		Seed: *seed, Bytes: *bytes, Branch: 0, VFR: true,
		Faults: fixture.FaultSpec{
			PCRWrap: true, CCJump: true, PSIBump: true, BadPSICRC: true, Truncate: true}})
	_, scanRep := ts.NewScanner().Scan(damaged)
	rep.DamageOffsetErrorBytes = abs64(scanRep.FirstFaultOffset - injected.First())
	damagedRes := fanout.Process(fanout.Branch{Name: "damaged", Data: damaged})
	rep.FaultsTotal += len(damagedRes.Faults)
	for _, f := range damagedRes.Faults {
		if !f.Kind.Recoverable() {
			rep.UnrecoverableSections++
		}
	}

	// Path 3: checkpointed recovery — interrupt, resume, repeat.
	store := recovery.Store{Dir: filepath.Join(*out, "checkpoints")}
	runner := recovery.Runner{Store: store, Every: 512}
	outTS := filepath.Join(*out, "recovered.ts")
	trusted, _ := recovery.TrustedPackets(damaged)
	crashAt := len(trusted) * 2 / 5
	if _, err := runner.Process("damaged", damaged, outTS, crashAt); err != nil {
		fmt.Fprintln(os.Stderr, "selfcheck recovery:", err)
		return exitConflict
	}
	start := time.Now()
	resumed, err := runner.Process("damaged", damaged, outTS, 0)
	rep.ResumeMs = float64(time.Since(start).Microseconds()) / 1000.0
	if err != nil || !resumed.Done {
		fmt.Fprintln(os.Stderr, "selfcheck resume:", err)
		return exitConflict
	}
	rep.SHA256 = resumed.SHA256
	repeat, err := runner.Process("damaged", damaged, outTS, 0)
	if err != nil {
		fmt.Fprintln(os.Stderr, "selfcheck repeat:", err)
		return exitConflict
	}
	rep.RepeatSHA256Equal = repeat.SHA256 == resumed.SHA256

	writeJSON(rep)
	if rep.UnrecoverableSections > 0 || rep.BoundaryConflicts > 0 ||
		rep.DamageOffsetErrorBytes > ts.PacketSize-1 || !rep.RepeatSHA256Equal {
		return exitConflict
	}
	return exitOK
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
