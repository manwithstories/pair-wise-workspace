// Command tscheck is the terminal entry point: it generates sample
// streams, runs checks and prints verification data as JSON.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"tscheck/internal/fanout"
	"tscheck/internal/fixture"
)

// Exit codes.
const (
	exitOK       = 0
	exitUsage    = 2
	exitConflict = 3
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(exitUsage)
	}
	var code int
	switch os.Args[1] {
	case "gen":
		code = cmdGen(os.Args[2:])
	case "check":
		code = cmdCheck(os.Args[2:])
	case "selfcheck":
		code = cmdSelfcheck(os.Args[2:])
	default:
		usage()
		code = exitUsage
	}
	os.Exit(code)
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  tscheck gen       --seed N --bytes N --branch 0..2 [--vfr] [--faults cc,crc,wrap,bump,truncate] --out file.ts
  tscheck check     --in file.ts
  tscheck selfcheck --seed N --bytes N --out dir`)
}

// writeJSON prints v as indented JSON on stdout.
func writeJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func cmdGen(args []string) int {
	fs := flag.NewFlagSet("gen", flag.ContinueOnError)
	seed := fs.Int64("seed", 1, "deterministic seed")
	bytes := fs.Int("bytes", 1<<20, "approximate size in bytes")
	branch := fs.Int("branch", 0, "bitrate branch 0..2")
	vfr := fs.Bool("vfr", false, "variable frame rate intervals")
	faults := fs.String("faults", "", "comma list: cc,crc,wrap,bump,truncate")
	out := fs.String("out", "", "output .ts file")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *out == "" || *bytes <= 0 || *branch < 0 || *branch > 2 {
		fmt.Fprintln(os.Stderr, "gen: invalid arguments")
		return exitUsage
	}
	spec := fixture.FaultSpec{}
	for _, f := range splitComma(*faults) {
		switch f {
		case "cc":
			spec.CCJump = true
		case "crc":
			spec.BadPSICRC = true
		case "wrap":
			spec.PCRWrap = true
		case "bump":
			spec.PSIBump = true
		case "truncate":
			spec.Truncate = true
		case "":
		default:
			fmt.Fprintf(os.Stderr, "gen: unknown fault %q\n", f)
			return exitUsage
		}
	}
	data, injected := fixture.Generate(fixture.Options{
		Seed: *seed, Bytes: *bytes, Branch: *branch, VFR: *vfr, Faults: spec})
	if err := os.WriteFile(*out, data, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "gen:", err)
		return exitUsage
	}
	writeJSON(map[string]any{
		"file": *out, "bytes": len(data),
		"first_fault_offset": injected.First(),
	})
	return exitOK
}

func cmdCheck(args []string) int {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	in := fs.String("in", "", "input .ts file")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *in == "" {
		fmt.Fprintln(os.Stderr, "check: --in required")
		return exitUsage
	}
	data, err := os.ReadFile(*in)
	if err != nil {
		fmt.Fprintln(os.Stderr, "check:", err)
		return exitUsage
	}
	res := fanout.Process(fanout.Branch{Name: "in", Data: data})
	faults := []map[string]any{}
	unrecoverable := 0
	for _, f := range res.Faults {
		faults = append(faults, map[string]any{
			"kind": f.Kind.String(), "offset": f.Offset, "pid": f.PID, "detail": f.Detail})
		if !f.Kind.Recoverable() {
			unrecoverable++
		}
	}
	writeJSON(map[string]any{
		"packets": res.Packets, "tracks": res.Tracks, "gop": res.GOP,
		"drift": res.Drift, "faults": faults, "unrecoverable": unrecoverable,
	})
	if unrecoverable > 0 {
		return exitConflict
	}
	return exitOK
}

func splitComma(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ',' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return out
}
