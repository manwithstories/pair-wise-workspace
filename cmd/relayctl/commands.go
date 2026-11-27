package main

import (
	"encoding/json"
	"fmt"
	"os"

	"relayctl/internal/cmaf"
	"relayctl/internal/fixture"
	"relayctl/internal/pipeline"
)

// render prints one JSON object per command, keeping output machine-readable.
func render(v any) {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "relayctl: render error: %v\n", err)
		return
	}
	fmt.Println(string(raw))
}

func errf(format string, a ...any) error {
	return fmt.Errorf(format, a...)
}

// cmdFixture generates a scenario and writes it to --out.
func cmdFixture(args []string) int {
	f, err := parseFlags(args)
	if err != nil {
		return usageErr("%v", err)
	}
	if bad := f.unknown("case", "seed", "out"); bad != "" {
		return usageErr("unknown flag --%s", bad)
	}
	caseName, code := f.required("case")
	if code != exitOK {
		return usageErr("--case is required")
	}
	seed, code := f.intOr("seed", 0)
	if code != exitOK {
		return usageErr("--seed must be an integer")
	}
	out, code := f.required("out")
	if code != exitOK {
		return usageErr("--out is required")
	}
	sc, err := fixture.Generate(caseName, int64(seed))
	if err != nil {
		return usageErr("%v", err)
	}
	if err := pipeline.SaveScenario(out, sc); err != nil {
		fmt.Fprintf(os.Stderr, "relayctl: %v\n", err)
		return exitLost
	}
	render(map[string]any{
		"case": caseName, "seed": seed, "out": out,
		"packets":           len(sc.Packets),
		"cmaf_bytes":        len(sc.CMAF),
		"expected_damage":   sc.Manifest.ExpectedDamageOffset,
		"unrecoverable":     sc.Manifest.ExpectUnrecoverable,
	})
	return exitOK
}

// cmdRun executes the graph, optionally pausing after a node.
func cmdRun(args []string) int {
	cfg, code := parseRunConfig(args)
	if code != exitOK {
		return code
	}
	eng, err := pipeline.NewEngine(cfg)
	if err != nil {
		return failRun(err)
	}
	st, err := eng.Run()
	render(st)
	return st.ExitCode
}

// cmdResume continues a paused/failed run.
func cmdResume(args []string) int {
	cfg, code := parseRunConfig(args)
	if code != exitOK {
		return code
	}
	eng, err := pipeline.NewEngine(cfg)
	if err != nil {
		return failRun(err)
	}
	st, err := eng.Resume()
	render(st)
	return st.ExitCode
}

// cmdVerify runs the graph then repeats manifest construction.
func cmdVerify(args []string) int {
	f, err := parseFlags(args)
	if err != nil {
		return usageErr("%v", err)
	}
	if bad := f.unknown("input", "state", "repeat"); bad != "" {
		return usageErr("unknown flag --%s", bad)
	}
	input, code := f.required("input")
	if code != exitOK {
		return usageErr("--input is required")
	}
	state, code := f.required("state")
	if code != exitOK {
		return usageErr("--state is required")
	}
	repeat, code := f.intOr("repeat", 1)
	if code != exitOK {
		return usageErr("--repeat must be an integer")
	}
	eng, err := pipeline.NewEngine(pipeline.Config{InputDir: input, StateDir: state})
	if err != nil {
		return failRun(err)
	}
	st, err := eng.Verify(repeat)
	if err != nil {
		render(st)
		return st.ExitCode
	}
	render(st)
	return st.ExitCode
}

// cmdInspect parses a damaged fixture directly and reports the located
// damage offset against the manifest's expected value (delta must be 0).
func cmdInspect(args []string) int {
	f, err := parseFlags(args)
	if err != nil {
		return usageErr("%v", err)
	}
	if bad := f.unknown("input"); bad != "" {
		return usageErr("unknown flag --%s", bad)
	}
	input, code := f.required("input")
	if code != exitOK {
		return usageErr("--input is required")
	}
	sc, err := pipeline.LoadScenario(input)
	if err != nil {
		fmt.Fprintf(os.Stderr, "relayctl: %v\n", err)
		return exitUsage
	}
	target := sc.CMAF
	if sc.DamagedCMAF != nil {
		target = sc.DamagedCMAF
	}
	_, _, perr := cmaf.ParseFragmentStream(target)
	found, ok := cmaf.DamageOffset(perr)
	out := map[string]any{
		"damage_expected_offset": sc.Manifest.ExpectedDamageOffset,
		"damage_found_offset":    found,
		"delta":                  found - sc.Manifest.ExpectedDamageOffset,
	}
	if !ok {
		out["damage_found_offset"] = -1
		out["delta"] = -1
		render(out)
		return exitOK
	}
	render(out)
	return exitDamaged
}

func parseRunConfig(args []string) (pipeline.Config, int) {
	f, err := parseFlags(args)
	if err != nil {
		return pipeline.Config{}, usageErr("%v", err)
	}
	if bad := f.unknown("input", "state", "pause-after"); bad != "" {
		return pipeline.Config{}, usageErr("unknown flag --%s", bad)
	}
	input, code := f.required("input")
	if code != exitOK {
		return pipeline.Config{}, usageErr("--input is required")
	}
	state, code := f.required("state")
	if code != exitOK {
		return pipeline.Config{}, usageErr("--state is required")
	}
	pause, _ := f.get("pause-after")
	return pipeline.Config{InputDir: input, StateDir: state, PauseAfter: pause}, exitOK
}

func failRun(err error) int {
	fmt.Fprintf(os.Stderr, "relayctl: %v\n", err)
	return exitLost
}
