// mp4tool inspects, seeks, hashes and exports MP4 files for the
// highlight-clip pipeline. Pure terminal UI, no external services.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"mp4tool/internal/box"
	"mp4tool/internal/index"
	"mp4tool/internal/timeline"
)

// Exit codes per spec.
const (
	exitOK       = 0
	exitUsage    = 2
	exitFile     = 3
	exitSeek     = 4
	exitSelftest = 5
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(exitUsage)
	}
	var code int
	switch os.Args[1] {
	case "fixtures":
		code = cmdFixtures(os.Args[2:])
	case "inspect":
		code = cmdInspect(os.Args[2:])
	case "seek":
		code = cmdSeek(os.Args[2:])
	case "hash":
		code = cmdHash(os.Args[2:])
	case "export":
		code = cmdExport(os.Args[2:])
	case "batch":
		code = cmdBatch(os.Args[2:])
	case "selftest":
		code = cmdSelftest(os.Args[2:])
	default:
		usage()
		code = exitUsage
	}
	os.Exit(code)
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: mp4tool <fixtures|inspect|seek|hash|export|batch|selftest> [flags]")
}

// fail maps an error to a message on stderr and an exit code.
func fail(err error) int {
	switch {
	case errors.Is(err, index.ErrSeekOutOfRange):
		fmt.Fprintln(os.Stderr, "error:", err)
		return exitSeek
	default:
		fmt.Fprintln(os.Stderr, "error:", err)
		return exitFile
	}
}

// pipeline bundles the shared parse state for one file.
type pipeline struct {
	f          *os.File
	moov       *box.Moov
	trak       *box.Trak
	frames     []index.Frame
	wrapEvents int
}

func (p *pipeline) close() { p.f.Close() }

func videoTrak(m *box.Moov) *box.Trak {
	for _, t := range m.Traks {
		if t.Handler == "vide" {
			return t
		}
	}
	if len(m.Traks) > 0 {
		return m.Traks[0]
	}
	return nil
}

// load parses moov (always) and builds the frame index (on demand).
func load(path string, needFrames bool) (*pipeline, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	moovHdr, total, err := box.FindMoov(f, uint64(fi.Size()))
	if err != nil {
		f.Close()
		return nil, err
	}
	m, err := box.ParseMoov(f, moovHdr)
	if err != nil {
		f.Close()
		return nil, err
	}
	m.BoxCount = total
	p := &pipeline{f: f, moov: m, trak: videoTrak(m)}
	if !needFrames {
		return p, nil
	}
	if p.trak == nil {
		f.Close()
		return nil, errors.New("no_track")
	}
	mapper := timeline.NewMapper(m.Timescale, p.trak.Timescale, p.trak.Elst)
	frames, cttsWraps, err := index.Build(p.trak, mapper, uint64(fi.Size()))
	if err != nil {
		f.Close()
		return nil, err
	}
	p.frames = frames
	p.wrapEvents = mapper.WrapEvents + cttsWraps
	return p, nil
}

func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	return fs
}
