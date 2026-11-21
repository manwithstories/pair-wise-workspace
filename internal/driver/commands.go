package driver

import (
	"context"
	"fmt"
	"os"

	"tsbatch/internal/fault"
	"tsbatch/internal/repack"
	"tsbatch/internal/sample"
)

// printReport prints the deterministic verification counters.
func printReport(prefix string, res *runResult) {
	r := &res.report
	fmt.Printf("%s_packets=%d\n", prefix, res.packets)
	fmt.Printf("%s_programs=%d\n", prefix, len(r.Programs))
	fmt.Printf("%s_keyframes=%d\n", prefix, len(r.Keyframes))
	fmt.Printf("%s_max_keyframe_interval_90k=%d\n", prefix, r.MaxKFInterval)
	fmt.Printf("%s_segments=%d\n", prefix, r.Segments)
	fmt.Printf("%s_isolations=%d\n", prefix, len(r.Isolations))
	fmt.Printf("%s_faults=%d\n", prefix, len(r.Faults))
	fmt.Printf("%s_pcr_wraps=%d\n", prefix, r.PCRWraps)
	fmt.Printf("%s_cc_gaps=%d\n", prefix, r.Faults.Count(fault.CatCCGap))
	fmt.Printf("%s_psi_updates=%d\n", prefix, r.PSIUpdates)
	fmt.Printf("%s_peak_queue=%d\n", prefix, res.peak)
	for _, f := range r.Faults {
		fmt.Printf("%s_fault %s\n", prefix, f)
	}
}

// exitCode maps a run result to the process exit code.
func exitCode(res *runResult) int {
	if res.readErr != nil {
		fmt.Fprintln(os.Stderr, "io error:", res.readErr)
		return ExitIO
	}
	if res.parseErr != nil || res.report.Faults.HasUnrecoverable() {
		fmt.Fprintln(os.Stderr, "unrecoverable stream error:", res.parseErr)
		return ExitStream
	}
	return ExitOK
}

func Check(args []string) int {
	pos := positional(args)
	if len(pos) != 1 {
		usage()
		return ExitArgs
	}
	workers, err1 := flagInt(args, "--workers", 4)
	queue, err2 := flagInt(args, "--queue", 8)
	if err1 != nil || err2 != nil || workers < 1 || queue < 1 {
		return ExitArgs
	}
	f, err := os.Open(pos[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, "io error:", err)
		return ExitIO
	}
	defer f.Close()
	w := repack.New(ioDiscard{}, repack.PolicyIsolate)
	res := runStream(context.Background(), f, w, workers, queue, GroupPackets, 1500)
	printReport("check", res)
	return exitCode(res)
}

func Repack(args []string) int {
	pos := positional(args)
	if len(pos) != 2 {
		usage()
		return ExitArgs
	}
	workers, err1 := flagInt(args, "--workers", 4)
	queue, err2 := flagInt(args, "--queue", 8)
	if err1 != nil || err2 != nil || workers < 1 || queue < 1 {
		return ExitArgs
	}
	policy := repack.PolicyIsolate
	if hasFlag(args, "--drop") {
		policy = repack.PolicyDrop
	}
	in, err := os.Open(pos[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, "io error:", err)
		return ExitIO
	}
	defer in.Close()
	fw, err := repack.Create(pos[1], pos[0], policy)
	if err != nil {
		fmt.Fprintln(os.Stderr, "io error:", err)
		return ExitIO
	}
	psiCC := map[uint16]uint8{}
	fw.PSIHook = psiHook(psiCC)
	res := runStream(context.Background(), in, fw.Writer, workers, queue, GroupPackets, 1500)
	if code := exitCode(res); code != ExitOK {
		fw.Abort()
		printReport("repack", res)
		return code
	}
	if err := fw.Close(); err != nil {
		fmt.Fprintln(os.Stderr, "io error:", err)
		return ExitIO
	}
	if err := repack.WriteManifest(pos[1]+".mp4", fw.Segments); err != nil {
		fmt.Fprintln(os.Stderr, "io error:", err)
		return ExitIO
	}
	printReport("repack", res)
	for _, s := range fw.Segments {
		fmt.Printf("segment_sha256[%d]=%s\n", s.ID, s.SHA256)
	}
	for _, iso := range fw.Isolations {
		fmt.Printf("isolation packets=%d-%d reason=%s dropped=%t\n",
			iso.StartPacket, iso.EndPacket, iso.Reason, iso.Dropped)
	}
	return ExitOK
}

// psiHook regenerates PAT/PMT packets at every segment boundary.
func psiHook(cc map[uint16]uint8) func(segID int) [][188]byte {
	return func(segID int) [][188]byte {
		pat := sample.PSIPacket(sample.PIDPAT, 0, cc[sample.PIDPAT])
		cc[sample.PIDPAT] = (cc[sample.PIDPAT] + 1) & 0x0F
		pmt := sample.PSIPacket(sample.PIDPMT, 0, cc[sample.PIDPMT])
		cc[sample.PIDPMT] = (cc[sample.PIDPMT] + 1) & 0x0F
		return [][188]byte{pat, pmt}
	}
}

type ioDiscard struct{}

func (ioDiscard) Write(p []byte) (int, error) { return len(p), nil }

func usage() {
	fmt.Println(`usage:
  tsbatch check  <in.ts> [--workers N] [--queue N]
  tsbatch repack <in.ts> <out.ts> [--drop] [--workers N] [--queue N]
  tsbatch demo   [--packets N] [--workers N] [--queue N] [--slow-consumer-ms N]`)
}

// flagInt scans args for --name and returns its value or def.
func flagInt(args []string, name string, def int) (int, error) {
	for i, a := range args {
		if a == name && i+1 < len(args) {
			var v int
			if _, err := fmt.Sscanf(args[i+1], "%d", &v); err != nil {
				return 0, fmt.Errorf("bad value for %s", name)
			}
			return v, nil
		}
	}
	return def, nil
}

func hasFlag(args []string, name string) bool {
	for _, a := range args {
		if a == name {
			return true
		}
	}
	return false
}

// positional returns non-flag arguments. valueFlags take a value; any
// other --flag is boolean.
func positional(args []string) []string {
	valueFlags := map[string]bool{
		"--workers": true, "--queue": true, "--packets": true,
		"--slow-consumer-ms": true,
	}
	var out []string
	for i := 0; i < len(args); i++ {
		if args[i][0] == '-' {
			if valueFlags[args[i]] {
				i++ // skip flag value
			}
			continue
		}
		out = append(out, args[i])
	}
	return out
}
