// mediac is the terminal-only batch QC and re-packaging tool for the
// broadcaster workflow: probe delivered MP4/MOV files, normalize
// representations, segment them at unified cut points, resume after
// interrupts, and verify the delivery manifest.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"mediac/internal/model"
	"mediac/internal/probe"
	"mediac/internal/segment"
	"mediac/internal/verify"
)

func main() { os.Exit(runCLI(os.Args[1:])) }

func runCLI(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: mediac <demo|resume|verify|probe> [--out DIR] [--interrupt-after N] [file]")
		return 1
	}
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	out := fs.String("out", "/tmp/media-demo", "output directory")
	interrupt := fs.Int("interrupt-after", 0, "simulate an interrupt after N segment tasks")
	if err := fs.Parse(args[1:]); err != nil {
		return 1
	}
	switch args[0] {
	case "demo":
		return cmdDemo(*out)
	case "resume":
		return cmdResume(*out, *interrupt)
	case "verify":
		return cmdVerify(*out)
	case "probe":
		if fs.NArg() != 1 {
			fmt.Fprintln(os.Stderr, "usage: mediac probe <file>")
			return 1
		}
		return cmdProbe(fs.Arg(0))
	}
	fmt.Fprintln(os.Stderr, "unknown command:", args[0])
	return 1
}

func cmdDemo(out string) int {
	s, _, merr := run(out, defaultConfig(), 0)
	if merr != nil {
		return fail(merr)
	}
	for _, name := range []string{
		"truncated_moov.mp4", "corrupt_mdat.mp4", "nonmonotonic.mp4", "unknown_codec.mp4"} {
		if _, me := probe.Probe(filepath.Join(out, "fixtures", name)); me != nil {
			s.Damaged = append(s.Damaged, *me)
		}
	}
	emit(s)
	return model.ExitOK
}

func cmdResume(out string, interrupt int) int {
	if interrupt > 0 {
		// Simulate a fresh shift: discard our own working state so the
		// interrupt and resume path is exercised end to end.
		for _, p := range []string{
			filepath.Join(out, "checkpoints"), filepath.Join(out, "segments"),
			filepath.Join(out, segment.Name)} {
			if err := os.RemoveAll(p); err != nil {
				return fail(model.New(model.StageRecovery, "reset_failed", err.Error(), 1))
			}
		}
	}
	s, done, merr := run(out, defaultConfig(), interrupt)
	if merr != nil {
		return fail(merr)
	}
	if done {
		emit(s)
		return model.ExitOK
	}
	emit(s) // interrupted state, checkpoints on disk
	start := time.Now()
	s2, _, merr := run(out, defaultConfig(), 0)
	if merr != nil {
		return fail(merr)
	}
	s2.ResumeMs = time.Since(start).Milliseconds()
	emit(s2)
	return model.ExitOK
}

func cmdVerify(out string) int {
	m, sha, merr := segment.Load(out)
	if merr != nil {
		return fail(merr)
	}
	if merr := verify.Hashes(out, m); merr != nil {
		return fail(merr)
	}
	delta, merr := verify.FanIn(m, 40)
	if merr != nil {
		return fail(merr)
	}
	n := 0
	for _, r := range m.Representations {
		n += len(r.Segments)
	}
	emit(&summary{Segments: n, BoundaryMaxDeltaMs: delta, SHA256: sha})
	return model.ExitOK
}

func cmdProbe(path string) int {
	res, merr := probe.Probe(path)
	if merr != nil {
		return fail(merr)
	}
	emit(res)
	return model.ExitOK
}

func emit(v interface{}) {
	b, _ := json.MarshalIndent(v, "", "  ")
	fmt.Println(string(b))
}

func fail(merr *model.MediaError) int {
	fmt.Fprintln(os.Stderr, merr.Error())
	return model.ExitCode(merr)
}
