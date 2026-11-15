// tsprobe is a terminal only MPEG-TS pre-flight tool for channel
// ingest: it scans multiplexes for table version changes, PID
// remapping, continuity counter anomalies and cuts per-program
// segments.
//
// Exit codes: 0 ok, 1 argument or I/O error, 2 sync loss or CRC over
// threshold, 3 unrecoverable program tables.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"tsprobe/internal/fixture"
	"tsprobe/internal/program"
	"tsprobe/internal/psi"
	"tsprobe/internal/report"
	"tsprobe/internal/segment"
	"tsprobe/internal/tsdemux"
)

const crcErrorThreshold = 16

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}
	var code int
	switch os.Args[1] {
	case "gen":
		code = cmdGen(os.Args[2:])
	case "scan":
		code = cmdScan(os.Args[2:])
	case "streams":
		code = cmdStreams(os.Args[2:])
	case "seg":
		code = cmdSeg(os.Args[2:])
	default:
		usage()
		code = 1
	}
	os.Exit(code)
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  tsprobe gen     --out f/mux.ts [--programs 2] [--seconds 30] [--drop 0.01] [--cc 7] [--pat 3] [--pmt 4]
  tsprobe scan    --in f/mux.ts [--json]
  tsprobe streams --in f/mux.ts [--json]
  tsprobe seg     --in f/mux.ts --program 1 --ms 4000 --out out
exit codes: 0 ok, 1 args/io, 2 sync loss or CRC over threshold, 3 unrecoverable tables`)
}

func cmdGen(args []string) int {
	fs := flag.NewFlagSet("gen", flag.ContinueOnError)
	opt := fixture.Options{}
	fs.StringVar(&opt.Out, "out", "", "output .ts file")
	fs.IntVar(&opt.Programs, "programs", 2, "number of programs")
	fs.IntVar(&opt.Seconds, "seconds", 30, "duration in seconds")
	fs.Float64Var(&opt.Drop, "drop", 0, "null packet drop probability")
	fs.IntVar(&opt.CCJumps, "cc", 0, "injected CC jumps")
	fs.IntVar(&opt.PATVersions, "pat", 1, "PAT versions cycled")
	fs.IntVar(&opt.PMTVersions, "pmt", 1, "PMT versions cycled per program")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if opt.Out == "" {
		fmt.Fprintln(os.Stderr, "gen: --out is required")
		return 1
	}
	sum, err := fixture.Generate(opt)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gen:", err)
		return 1
	}
	fmt.Println("generated", opt.Out)
	fmt.Println(sum)
	return 0
}

// pipeline is the shared single pass over the input.
type pipeline struct {
	dm  *tsdemux.Demuxer
	cc  *tsdemux.CCTracker
	col *report.Collector
	st  *program.State
	crc int64
	seg *segment.Segmenter
}

// runPipeline performs one streaming pass. When segOut is non-empty a
// segmenter for segProg is attached to the same program table view.
func runPipeline(input string, withTables bool, segProg uint16, segMS int64, segOut string) (*pipeline, error) {
	f, err := os.Open(input)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	p := &pipeline{
		dm:  tsdemux.New(f),
		cc:  tsdemux.NewCCTracker(),
		col: report.NewCollector(),
		st:  program.NewState(),
	}
	if segOut != "" {
		p.seg = segment.New(segOut, segProg, segMS, p.st)
	}
	patAsm := &psi.Assembler{}
	pmtAsms := map[uint16]*psi.Assembler{}
	for {
		pkt, err := p.dm.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		p.cc.Observe(pkt)
		p.col.Observe(pkt)
		tableChanged := false
		if withTables && !pkt.TEI && !pkt.Scrambled && pkt.HasPayload {
			switch {
			case pkt.PID == 0:
				before := len(p.st.Events)
				patAsm.Write(pkt.PUSI, pkt.Payload, func(sec []byte) {
					p.st.FeedPAT(sec, pkt.Index)
				})
				tableChanged = len(p.st.Events) > before
			default:
				if _, ok := p.st.PMTOwner[pkt.PID]; ok {
					asm := pmtAsms[pkt.PID]
					if asm == nil {
						asm = &psi.Assembler{}
						pmtAsms[pkt.PID] = asm
					}
					before := len(p.st.Events)
					pid := pkt.PID
					asm.Write(pkt.PUSI, pkt.Payload, func(sec []byte) {
						p.st.FeedPMT(pid, sec, pkt.Index)
					})
					tableChanged = len(p.st.Events) > before
				}
			}
		}
		if p.seg != nil {
			if err := p.seg.Observe(pkt, tableChanged); err != nil {
				return nil, err
			}
		}
	}
	p.crc = patAsm.CRCErrors
	for _, a := range pmtAsms {
		p.crc += a.CRCErrors
	}
	return p, nil
}

// exitCode maps pipeline problems to process exit codes.
func exitCode(p *pipeline) int {
	problems := p.st.Validate()
	for prog, streams := range p.st.Streams {
		for _, s := range streams {
			if st := p.col.Stat(s.PID); st == nil || st.Packets == 0 {
				problems = append(problems,
					fmt.Sprintf("program %d: elementary PID 0x%04X never seen", prog, s.PID))
			}
		}
	}
	for _, pr := range problems {
		fmt.Fprintln(os.Stderr, "unrecoverable:", pr)
	}
	if len(problems) > 0 {
		return 3
	}
	if p.dm.Stats.SyncLoss > 0 || p.crc > crcErrorThreshold {
		return 2
	}
	return 0
}

func cmdScan(args []string) int {
	fs := flag.NewFlagSet("scan", flag.ContinueOnError)
	in := fs.String("in", "", "input .ts file")
	asJSON := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(args); err != nil || *in == "" {
		usage()
		return 1
	}
	p, err := runPipeline(*in, true, 0, 0, "")
	if err != nil {
		fmt.Fprintln(os.Stderr, "scan:", err)
		return 1
	}
	rep := report.Build(*in, p.dm, p.cc, p.col, p.st, p.crc)
	if *asJSON {
		if err := rep.JSON(os.Stdout); err != nil {
			return 1
		}
	} else {
		rep.Text(os.Stdout)
	}
	return exitCode(p)
}

func cmdStreams(args []string) int {
	fs := flag.NewFlagSet("streams", flag.ContinueOnError)
	in := fs.String("in", "", "input .ts file")
	asJSON := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(args); err != nil || *in == "" {
		usage()
		return 1
	}
	p, err := runPipeline(*in, false, 0, 0, "")
	if err != nil {
		fmt.Fprintln(os.Stderr, "streams:", err)
		return 1
	}
	rep := report.Build(*in, p.dm, p.cc, p.col, p.st, 0)
	if *asJSON {
		if err := rep.JSON(os.Stdout); err != nil {
			return 1
		}
	} else {
		rep.Text(os.Stdout)
	}
	if p.dm.Stats.SyncLoss > 0 {
		return 2
	}
	return 0
}

func cmdSeg(args []string) int {
	fs := flag.NewFlagSet("seg", flag.ContinueOnError)
	in := fs.String("in", "", "input .ts file")
	prog := fs.Int("program", 0, "program number to cut")
	ms := fs.Int64("ms", 4000, "segment length in milliseconds")
	out := fs.String("out", "", "output directory")
	if err := fs.Parse(args); err != nil || *in == "" || *out == "" || *prog <= 0 || *ms <= 0 {
		usage()
		return 1
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "seg:", err)
		return 1
	}
	p, err := runPipeline(*in, true, uint16(*prog), *ms, *out)
	if err != nil {
		fmt.Fprintln(os.Stderr, "seg:", err)
		return 1
	}
	if _, ok := p.st.PMTPID[uint16(*prog)]; !ok {
		fmt.Fprintf(os.Stderr, "seg: program %d not present in PAT\n", *prog)
		return 1
	}
	m, err := p.seg.Finish(*in)
	if err != nil {
		fmt.Fprintln(os.Stderr, "seg:", err)
		return 1
	}
	fmt.Printf("segments: %d -> %s\n", len(m.Segments), *out)
	for _, s := range m.Segments {
		fmt.Printf("  %s %d..%d ms pat_v%d packets=%d streams=%d\n",
			s.File, s.StartMS, s.EndMS, s.PATVersion, s.Packets, len(s.Streams))
	}
	return exitCode(p)
}
