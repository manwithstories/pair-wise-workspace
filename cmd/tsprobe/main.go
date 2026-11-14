// tsprobe is a terminal tool for MPEG-TS program table probing,
// continuity analysis, slicing and fixture verification.
package main

import (
	"flag"
	"fmt"
	"os"

	"tsprobe/internal/fixture"
	"tsprobe/internal/report"
	"tsprobe/internal/slicer"
)

// Exit codes.
const (
	exitOK       = 0
	exitIO       = 1 // parameter or IO failure
	exitNoPAT    = 2 // no valid PAT within the first 1024 packets
	exitMismatch = 3 // counts differ from expectations
	exitManifest = 4 // manifest does not match actual segments
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		usage()
		return exitIO
	}
	switch args[0] {
	case "gen":
		return cmdGen(args[1:])
	case "scan":
		return cmdScan(args[1:])
	case "slice":
		return cmdSlice(args[1:])
	case "verify":
		return cmdVerify(args[1:])
	default:
		usage()
		return exitIO
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: tsprobe <gen|scan|slice|verify> [flags]")
}

func fixtureParams(fs *flag.FlagSet) *fixture.Params {
	p := &fixture.Params{}
	fs.Uint64Var(&p.Seed, "seed", 0, "PRNG seed")
	fs.IntVar(&p.Programs, "programs", 3, "number of programs")
	fs.IntVar(&p.Seconds, "seconds", 30, "stream duration in seconds")
	fs.IntVar(&p.PATVersions, "pat-versions", 4, "PAT version count")
	fs.IntVar(&p.PMTVersions, "pmt-versions", 6, "PMT version count per program")
	fs.IntVar(&p.InjectLoss, "inject-loss", 0, "ES packets to drop")
	fs.IntVar(&p.InjectCCJump, "inject-cc-jump", 0, "CC jump events to inject")
	return p
}

func cmdGen(args []string) int {
	fs := flag.NewFlagSet("gen", flag.ContinueOnError)
	out := fs.String("out", "", "output .ts file")
	wrap192 := fs.Bool("wrap-192", false, "write 192-byte (4-byte prefix) packets")
	p := fixtureParams(fs)
	if err := fs.Parse(args); err != nil {
		return exitIO
	}
	if *out == "" {
		fmt.Fprintln(os.Stderr, "gen: --out is required")
		return exitIO
	}
	f, err := os.Create(*out)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gen:", err)
		return exitIO
	}
	defer f.Close()
	p.Wrap192 = *wrap192
	plan, err := fixture.Generate(f, *p)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gen:", err)
		return exitIO
	}
	fmt.Printf("wrote %s: packets=%d es=%d null=%d psi=%d lost=%d cc_jumps=%d "+
		"pat_changes=%d pmt_changes/program=%d\n",
		*out, plan.Packets, plan.ESPackets, plan.NullPackets, plan.PSIPackets,
		plan.LostEst, plan.JumpEvents, plan.PATChanges, p.PMTVersions-1)
	return exitOK
}

func cmdScan(args []string) int {
	fs := flag.NewFlagSet("scan", flag.ContinueOnError)
	in := fs.String("in", "", "input .ts file")
	jsonOut := fs.String("json", "", "write JSON report to this path")
	if err := fs.Parse(args); err != nil {
		return exitIO
	}
	if *in == "" {
		fmt.Fprintln(os.Stderr, "scan: --in is required")
		return exitIO
	}
	f, err := os.Open(*in)
	if err != nil {
		fmt.Fprintln(os.Stderr, "scan:", err)
		return exitIO
	}
	defer f.Close()
	rep, err := report.Scan(f, *in)
	if err != nil {
		fmt.Fprintln(os.Stderr, "scan:", err)
		return exitIO
	}
	rep.WriteText(os.Stdout)
	if *jsonOut != "" {
		if err := rep.WriteJSON(*jsonOut); err != nil {
			fmt.Fprintln(os.Stderr, "scan:", err)
			return exitIO
		}
	}
	if rep.PATFirstPacket < 0 || rep.PATFirstPacket >= 1024 {
		fmt.Fprintln(os.Stderr, "scan: no valid PAT within the first 1024 packets")
		return exitNoPAT
	}
	return exitOK
}

func cmdSlice(args []string) int {
	fs := flag.NewFlagSet("slice", flag.ContinueOnError)
	in := fs.String("in", "", "input .ts file")
	outDir := fs.String("out-dir", "", "segment output directory")
	gopBytes := fs.Uint64("gop-bytes", 1048576, "target bytes per segment")
	manifest := fs.String("manifest", "", "manifest JSON path")
	if err := fs.Parse(args); err != nil {
		return exitIO
	}
	if *in == "" || *outDir == "" {
		fmt.Fprintln(os.Stderr, "slice: --in and --out-dir are required")
		return exitIO
	}
	f, err := os.Open(*in)
	if err != nil {
		fmt.Fprintln(os.Stderr, "slice:", err)
		return exitIO
	}
	defer f.Close()
	m, err := slicer.New().Slice(f, *in, *outDir, *manifest, *gopBytes)
	if err != nil {
		fmt.Fprintln(os.Stderr, "slice:", err)
		return exitIO
	}
	var esTotal uint64
	for _, s := range m.Segments {
		for pid, n := range s.PIDCounts {
			if pid != 0 && pid != 0x1FFF && (pid < 0x0100 || pid > 0x01FF) {
				esTotal += n
			}
		}
	}
	fmt.Printf("slices=%d es_packets_sum=%d manifest=%s\n", len(m.Segments), esTotal, *manifest)
	return exitOK
}
