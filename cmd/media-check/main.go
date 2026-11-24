// media-check is the batch acceptance entry point for the broadcast
// master-archive ingest pipeline. Terminal-only; all behaviour is
// driven by run arguments and stdout.
//
// Exit codes:
//
//	0  processing and verification completed
//	2  input or argument error
//	3  located corruption exists; other segments completed
//	4  kernel or verification failure
package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"media-check/internal/adapter"
	"media-check/internal/adapter/audio"
	"media-check/internal/adapter/frame"
	mp4adapter "media-check/internal/adapter/mp4"
	"media-check/internal/pipeline"
)

const (
	exitOK         = 0
	exitUsage      = 2
	exitCorruption = 3
	exitKernel     = 4
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		usage()
		return exitUsage
	}
	switch args[0] {
	case "demo":
		return runDemo(args[1:])
	case "adapters":
		reg := defaultRegistry()
		for _, k := range reg.List() {
			fmt.Println("adapter=" + k)
		}
		return exitOK
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", args[0])
		usage()
		return exitUsage
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  media-check demo --frames N --segment-frames M --inject a,b,c --repeat R
  media-check adapters

injectors: timestamp-wrap, box-truncate, audio-conflict, hash-corrupt`)
}

func defaultRegistry() *adapter.Registry {
	reg := adapter.NewRegistry()
	reg.Register(frame.Adapter{})
	reg.Register(mp4adapter.Adapter{})
	reg.Register(audio.Adapter{})
	return reg
}

func newPipeline() *pipeline.Pipeline {
	return pipeline.NewPipeline(defaultRegistry(), pipeline.NewStore())
}

// demoFlags holds parsed demo arguments.
type demoFlags struct {
	frames        int
	segmentFrames int
	inject        map[string]bool
	repeat        int
}

var knownInjectors = []string{
	"timestamp-wrap", "box-truncate", "audio-conflict", "hash-corrupt",
}

func parseDemoFlags(args []string) (demoFlags, error) {
	f := demoFlags{frames: 1200, segmentFrames: 120, repeat: 1, inject: map[string]bool{}}
	for i := 0; i < len(args); i++ {
		a := args[i]
		next := func() (string, error) {
			if i+1 >= len(args) {
				return "", fmt.Errorf("flag %s needs a value", a)
			}
			i++
			return args[i], nil
		}
		switch {
		case a == "--frames":
			v, err := next()
			if err != nil {
				return f, err
			}
			n, err := strconv.Atoi(v)
			if err != nil || n <= 0 || n > 1<<20 {
				return f, fmt.Errorf("--frames: invalid %q", v)
			}
			f.frames = n
		case a == "--segment-frames":
			v, err := next()
			if err != nil {
				return f, err
			}
			n, err := strconv.Atoi(v)
			if err != nil || n <= 0 || n > f.frames {
				return f, fmt.Errorf("--segment-frames: invalid %q", v)
			}
			f.segmentFrames = n
		case a == "--repeat":
			v, err := next()
			if err != nil {
				return f, err
			}
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > 16 {
				return f, fmt.Errorf("--repeat: invalid %q", v)
			}
			f.repeat = n
		case a == "--inject":
			v, err := next()
			if err != nil {
				return f, err
			}
			if v == "" || v == "none" {
				continue
			}
			for _, name := range strings.Split(v, ",") {
				ok := false
				for _, k := range knownInjectors {
					if name == k {
						ok = true
					}
				}
				if !ok {
					return f, fmt.Errorf("unknown injector %q", name)
				}
				f.inject[name] = true
			}
		default:
			return f, fmt.Errorf("unknown argument %q", a)
		}
	}
	return f, nil
}

// runDemo is filled in by the follow-up commit.
func runDemo(args []string) int {
	if _, err := parseDemoFlags(args); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return exitUsage
	}
	fmt.Fprintln(os.Stderr, "demo not yet implemented")
	return exitKernel
}
