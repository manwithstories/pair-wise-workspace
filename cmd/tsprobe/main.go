package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/asticode/go-astits"

	"tsprobe/internal/es"
	"tsprobe/internal/fixture"
	"tsprobe/internal/psi"
	"tsprobe/internal/report"
	"tsprobe/internal/slicer"
	"tsprobe/internal/tspkt"
)

const (
	exitOK       = 0
	exitRuntime  = 1
	exitNoTables = 2
	exitAnomaly  = 3
	exitUsage    = 4
)

func main() {
	os.Exit(run(os.Args))
}

func run(args []string) int {
	if len(args) < 2 {
		printUsage()
		return exitUsage
	}
	switch args[1] {
	case "gen":
		return runGen(args[2:])
	case "scan":
		return runScan(args[2:])
	case "slice":
		return runSlice(args[2:])
	case "selfcheck":
		return runSelfcheck(args[2:])
	case "help", "-h", "--help":
		printUsage()
		return exitOK
	default:
		fmt.Fprintf(os.Stderr, "tsprobe: unknown command %q\n", args[1])
		return exitUsage
	}
}

func printUsage() {
	fmt.Fprintln(os.Stderr, "usage: tsprobe <gen|scan|slice|selfcheck> [flags]")
	fmt.Fprintln(os.Stderr, "  gen        generate a deterministic MPEG-TS fixture")
	fmt.Fprintln(os.Stderr, "  scan       inspect PAT/PMT, PID and continuity accounting")
	fmt.Fprintln(os.Stderr, "  slice      build a PCR-driven segment index")
	fmt.Fprintln(os.Stderr, "  selfcheck  regenerate and verify injected anomalies")
}

func flagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	return fs
}

func parseFlags(fs *flag.FlagSet, args []string) (int, bool) {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK, false
		}
		return exitUsage, false
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(os.Stderr, "tsprobe %s: unexpected arguments: %v\n", fs.Name(), fs.Args())
		return exitUsage, false
	}
	return exitOK, true
}

func runGen(args []string) int {
	fs := flagSet("gen")
	out := fs.String("out", "", "output TS path")
	seed := fs.Int64("seed", 7, "deterministic seed")
	programs := fs.Int("programs", 2, "number of programs")
	streams := fs.Int("streams", 3, "elementary streams per program")
	seconds := fs.Int("seconds", 20, "duration in seconds")
	drop := fs.Int("drop", 0, "packets omitted from the fixture")
	ccJump := fs.Int("cc-jump", 0, "backward continuity-count injections")
	revisions := fs.Int("psi-revisions", 2, "PAT and PMT revisions")
	packetSize := fs.Int("packet-size", 188, "TS packet size (188, 192 or 204)")
	if code, ok := parseFlags(fs, args); !ok {
		return code
	}
	if *out == "" || *programs < 1 || *streams < 1 || *seconds < 1 || *drop < 0 || *ccJump < 0 || *revisions < 1 || *revisions > 31 {
		fmt.Fprintln(os.Stderr, "tsprobe gen: invalid argument range")
		return exitUsage
	}
	if *packetSize != 188 && *packetSize != 192 && *packetSize != 204 {
		fmt.Fprintln(os.Stderr, "tsprobe gen: packet-size must be 188, 192 or 204")
		return exitUsage
	}
	if dir := filepath.Dir(*out); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "tsprobe gen: %v\n", err)
			return exitRuntime
		}
	}
	result, err := fixture.GenerateFile(*out, fixture.Options{
		Seed: *seed, Programs: *programs, Streams: *streams, Seconds: *seconds,
		Drop: *drop, CCJump: *ccJump, PSIRevisions: *revisions, PacketSize: *packetSize,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "tsprobe gen: %v\n", err)
		return exitRuntime
	}
	fmt.Printf("generated=%s packets=%d dropped=%d cc_ahead=%d cc_backwards=%d pat_revisions=%d pmt_revisions=%v packet_size=%d\n",
		*out, result.WrittenPackets, result.DroppedPackets, result.CCAhead, result.CCBackwards,
		result.PATRevisions, result.PMTRevisions, result.PacketSize)
	return exitOK
}

type analysisResult struct {
	report report.Report
	code   int
}

func runScan(args []string) int {
	fs := flagSet("scan")
	input := fs.String("input", "", "input TS path")
	asJSON := fs.Bool("json", false, "write JSON")
	asTable := fs.Bool("table", false, "write table")
	if code, ok := parseFlags(fs, args); !ok {
		return code
	}
	if *input == "" || (*asJSON && *asTable) {
		fmt.Fprintln(os.Stderr, "tsprobe scan: input is required and output modes are exclusive")
		return exitUsage
	}
	result, err := analyze(*input, 0, false)
	if err != nil {
		fmt.Fprintf(os.Stderr, "tsprobe scan: %v\n", err)
		return exitRuntime
	}
	if err := writeReport(os.Stdout, result.report, *asJSON); err != nil {
		fmt.Fprintf(os.Stderr, "tsprobe scan: %v\n", err)
		return exitRuntime
	}
	return result.code
}

func runSlice(args []string) int {
	fs := flagSet("slice")
	input := fs.String("input", "", "input TS path")
	segment := fs.Duration("segment", 0, "target segment duration")
	outDir := fs.String("out-dir", "", "segment index directory")
	asJSON := fs.Bool("json", false, "write JSON")
	asTable := fs.Bool("table", false, "write table")
	if code, ok := parseFlags(fs, args); !ok {
		return code
	}
	if *input == "" || *segment <= 0 || *outDir == "" || (*asJSON && *asTable) {
		fmt.Fprintln(os.Stderr, "tsprobe slice: input, positive segment and out-dir are required")
		return exitUsage
	}
	result, err := analyze(*input, *segment, true)
	if err != nil {
		fmt.Fprintf(os.Stderr, "tsprobe slice: %v\n", err)
		return exitRuntime
	}
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "tsprobe slice: %v\n", err)
		return exitRuntime
	}
	indexPath := filepath.Join(*outDir, "index.json")
	f, err := os.Create(indexPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "tsprobe slice: %v\n", err)
		return exitRuntime
	}
	writeErr := report.WriteJSON(f, result.report)
	closeErr := f.Close()
	if writeErr != nil {
		fmt.Fprintf(os.Stderr, "tsprobe slice: %v\n", writeErr)
		return exitRuntime
	}
	if closeErr != nil {
		fmt.Fprintf(os.Stderr, "tsprobe slice: %v\n", closeErr)
		return exitRuntime
	}
	if err := writeReport(os.Stdout, result.report, *asJSON); err != nil {
		fmt.Fprintf(os.Stderr, "tsprobe slice: %v\n", err)
		return exitRuntime
	}
	return result.code
}

func writeReport(w io.Writer, r report.Report, asJSON bool) error {
	if asJSON {
		return report.WriteJSON(w, r)
	}
	return report.WriteTable(w, r)
}

func analyze(path string, target time.Duration, captureSegments bool) (analysisResult, error) {
	f, err := os.Open(path)
	if err != nil {
		return analysisResult{}, err
	}
	defer f.Close()
	reader, err := tspkt.NewReader(f)
	if errors.Is(err, tspkt.ErrNoSync) {
		registry := psi.NewRegistry()
		counter := es.NewCounter()
		empty := report.Build(report.BuildInput{
			Status: "no_pat", Input: path, Registry: registry, Counter: counter,
			Segments: []slicer.Segment{},
		})
		return analysisResult{report: empty, code: exitNoTables}, nil
	}
	if err != nil {
		return analysisResult{}, err
	}
	assembler := psi.NewAssembler()
	tables := psi.NewTableState()
	registry := psi.NewRegistry()
	counter := es.NewCounter()
	var builder *slicer.Builder
	if captureSegments {
		builder = slicer.NewBuilder(target, registry)
	}
	lastPacket := int64(-1)
	for {
		packet, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return analysisResult{}, err
		}
		lastPacket = packet.Index
		isPSI := packet.PID == astits.PIDPAT || registry.IsPSIPID(packet.PID)
		if packet.HasPayload && isPSI {
			sections, err := assembler.Push(packet.PID, packet.Payload, packet.PUSI, packet.Index)
			if err != nil {
				return analysisResult{}, err
			}
			for _, section := range sections {
				changes, err := tables.Consume(section)
				if err != nil {
					return analysisResult{}, err
				}
				for _, change := range changes {
					if _, err := registry.Apply(change); err != nil {
						return analysisResult{}, err
					}
				}
			}
			isPSI = packet.PID == astits.PIDPAT || registry.IsPSIPID(packet.PID)
		}
		assignment, assigned := registry.LookupPID(packet.PID)
		if err := counter.Observe(*packet, assignment, assigned, isPSI); err != nil {
			return analysisResult{}, err
		}
		if builder != nil {
			builder.Observe(*packet)
		}
	}
	assembler.Finish()
	counter.Finish()
	segments := []slicer.Segment{}
	if builder != nil {
		segments = builder.Finish(lastPacket)
	}
	status, code := classify(reader.Stats(), assembler.Stats(), registry, counter)
	out := report.Build(report.BuildInput{
		Status: status, Input: path, Reader: reader.Stats(), Sections: assembler.Stats(),
		Registry: registry, Counter: counter, Segments: segments,
	})
	return analysisResult{report: out, code: code}, nil
}

func classify(read tspkt.Stats, sections psi.SectionStats, registry *psi.Registry, counter *es.Counter) (string, int) {
	if sections.CRCErrors > 0 && sections.CRCOk == 0 {
		return "all_crc_failed", exitNoTables
	}
	if registry.PATRevisions() == 0 {
		return "no_pat", exitNoTables
	}
	totals := counter.Totals()
	if totals.CCAhead > 0 || totals.CCBackwards > 0 || totals.Duplicates > 0 ||
		totals.PESTruncated > 0 || sections.CRCErrors > 0 || sections.TruncatedSections > 0 ||
		read.ResyncEvents > 0 || read.TruncatedPackets > 0 {
		return "anomalies", exitAnomaly
	}
	return "ok", exitOK
}

func runSelfcheck(args []string) int {
	fs := flagSet("selfcheck")
	seed := fs.Int64("seed", 7, "deterministic seed")
	programs := fs.Int("programs", 2, "number of programs")
	streams := fs.Int("streams", 3, "elementary streams per program")
	seconds := fs.Int("seconds", 20, "duration in seconds")
	drop := fs.Int("drop", 5, "packets omitted from the fixture")
	ccJump := fs.Int("cc-jump", 3, "backward CC injections")
	revisions := fs.Int("psi-revisions", 2, "PAT and PMT revisions")
	segment := fs.Duration("segment", 4*time.Second, "segment target for verification")
	packetSize := fs.Int("packet-size", 188, "TS packet size")
	if code, ok := parseFlags(fs, args); !ok {
		return code
	}
	if *programs < 1 || *streams < 1 || *seconds < 1 || *drop < 0 || *ccJump < 0 ||
		*revisions < 1 || *revisions > 31 || *segment <= 0 || (*packetSize != 188 && *packetSize != 192 && *packetSize != 204) {
		fmt.Fprintln(os.Stderr, "tsprobe selfcheck: invalid argument range")
		return exitUsage
	}
	tmp, err := os.CreateTemp("", "tsprobe-selfcheck-*.ts")
	if err != nil {
		fmt.Fprintf(os.Stderr, "tsprobe selfcheck: %v\n", err)
		return exitRuntime
	}
	path := tmp.Name()
	_ = tmp.Close()
	defer os.Remove(path)
	fixtureResult, err := fixture.GenerateFile(path, fixture.Options{
		Seed: *seed, Programs: *programs, Streams: *streams, Seconds: *seconds,
		Drop: *drop, CCJump: *ccJump, PSIRevisions: *revisions, PacketSize: *packetSize,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "tsprobe selfcheck: generate: %v\n", err)
		return exitRuntime
	}
	analysis, err := analyze(path, *segment, true)
	if err != nil {
		fmt.Fprintf(os.Stderr, "tsprobe selfcheck: analyze: %v\n", err)
		return exitRuntime
	}
	checks := []struct {
		name string
		ok   bool
		want any
		got  any
	}{
		{"packet_size", analysis.report.PacketSize == fixtureResult.PacketSize, fixtureResult.PacketSize, analysis.report.PacketSize},
		{"packets_total", analysis.report.PacketsTotal == fixtureResult.WrittenPackets, fixtureResult.WrittenPackets, analysis.report.PacketsTotal},
		{"cc_ahead", analysis.report.CCAhead == int64(fixtureResult.CCAhead), fixtureResult.CCAhead, analysis.report.CCAhead},
		{"cc_backwards", analysis.report.CCBackwards == int64(fixtureResult.CCBackwards), fixtureResult.CCBackwards, analysis.report.CCBackwards},
		{"duplicates", analysis.report.Duplicates == 0, 0, analysis.report.Duplicates},
		{"pat_revisions", analysis.report.PAT.Revisions == fixtureResult.PATRevisions, fixtureResult.PATRevisions, analysis.report.PAT.Revisions},
		{"programs", len(analysis.report.Programs) == *programs, *programs, len(analysis.report.Programs)},
	}
	esPackets := int64(0)
	for i, program := range analysis.report.Programs {
		for _, stream := range program.ES {
			esPackets += stream.Packets
		}
		want := 0
		if i < len(fixtureResult.PMTRevisions) {
			want = fixtureResult.PMTRevisions[i]
		}
		checks = append(checks, struct {
			name string
			ok   bool
			want any
			got  any
		}{fmt.Sprintf("program_%d_pmt_revisions", program.ProgramNumber), program.PMTRevisions == want, want, program.PMTRevisions})
	}
	esSum := esPackets + analysis.report.NullPackets + analysis.report.PSIPackets
	checks = append(checks, struct {
		name string
		ok   bool
		want any
		got  any
	}{"packet_accounting", esSum == analysis.report.PacketsTotal, analysis.report.PacketsTotal, esSum})
	wantSegments := int((int64(*seconds)*int64(time.Second) + int64(*segment) - 1) / int64(*segment))
	checks = append(checks, struct {
		name string
		ok   bool
		want any
		got  any
	}{"segments", len(analysis.report.Segments) == wantSegments, wantSegments, len(analysis.report.Segments)})
	for _, segment := range analysis.report.Segments {
		checks = append(checks, struct {
			name string
			ok   bool
			want any
			got  any
		}{fmt.Sprintf("segment_%d_random_access", segment.Index), segment.StartRandomAccess, true, segment.StartRandomAccess})
	}
	failed := false
	for _, check := range checks {
		state := "PASS"
		if !check.ok {
			state = "FAIL"
			failed = true
		}
		fmt.Printf("%s %-28s want=%v got=%v\n", state, check.name, check.want, check.got)
	}
	if failed {
		return exitRuntime
	}
	fmt.Printf("selfcheck ok seed=%d programs=%d streams=%d packets=%d segments=%d\n",
		*seed, *programs, *streams, analysis.report.PacketsTotal, len(analysis.report.Segments))
	return exitOK
}
