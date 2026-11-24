// Command mediad is the terminal acceptance entry point. It generates the
// self-contained fixtures, runs the pipeline over them, resumes after a
// simulated interruption and prints the metric table the contract specifies.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"mediad/internal/model"
	"mediad/internal/pipeline"
	"mediad/internal/synth"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		usage()
		return 2
	}
	switch args[0] {
	case "demo":
		return demo(args[1:])
	case "help", "-h", "--help":
		usage()
		return 0
	default:
		fmt.Fprintf(os.Stderr, "mediad: unknown command %q\n", args[0])
		usage()
		return 2
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `mediad — remote production media recovery (terminal only)

usage:
  mediad demo [flags]

flags:
  --tracks N            number of source tracks (default 3)
  --frames N            frames per track (default 240)
  --corrupt N           corrupt payloads per track (default 0)
  --truncate N          tracks with a torn tail frame (default 0)
  --partial-index       chop each index mid-entry before resume
  --interrupt-after N   stop each track after N frames, then resume
  --resume              resume from a committed checkpoint (implied by interrupt)
  --rtp-loss N          preview packets lost per track (manifest gaps)
  --work DIR            work directory (default: a fresh temp dir)
  --keep                keep the work directory after the run

exit codes: 0 complete and within bounds; 2 unrecoverable input; 3 failed check
`)
}

type demoFlags struct {
	tracks     int
	frames     int
	corrupt    int
	truncate   int
	partialIdx bool
	interrupt  int
	rtpLoss    int
	resume     bool
	work       string
	keep       bool
}

func demo(args []string) int {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	var f demoFlags
	fs.IntVar(&f.tracks, "tracks", 3, "number of tracks")
	fs.IntVar(&f.frames, "frames", 240, "frames per track")
	fs.IntVar(&f.corrupt, "corrupt", 0, "corrupt payloads per track")
	fs.IntVar(&f.truncate, "truncate", 0, "tracks with a torn tail")
	fs.BoolVar(&f.partialIdx, "partial-index", false, "chop index before resume")
	fs.IntVar(&f.interrupt, "interrupt-after", 0, "stop after this many frames")
	fs.IntVar(&f.rtpLoss, "rtp-loss", 0, "preview packets lost per track")
	fs.BoolVar(&f.resume, "resume", false, "resume after interruption (implied by --interrupt-after)")
	fs.StringVar(&f.work, "work", "", "work directory")
	fs.BoolVar(&f.keep, "keep", false, "keep work directory")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if f.tracks <= 0 || f.frames <= 0 {
		fmt.Fprintln(os.Stderr, "mediad: tracks and frames must be positive")
		return 2
	}

	work := f.work
	if work == "" {
		d, err := os.MkdirTemp("", "mediad-demo-*")
		if err != nil {
			fmt.Fprintf(os.Stderr, "mediad: %v\n", err)
			return 2
		}
		work = d
	}
	if !f.keep && f.work == "" {
		defer os.RemoveAll(work)
	}
	if err := os.MkdirAll(work, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "mediad: %v\n", err)
		return 2
	}

	results, code := runDemo(work, f)
	if code != 0 && len(results) == 0 {
		return code
	}
	printTable(results)
	return code
}

// runDemo builds every track's source, runs the pipeline, and — when an
// interruption is requested — reruns it in resume mode. It returns the per
// track results in track order.
func runDemo(work string, f demoFlags) ([]pipeline.TrackResult, int) {
	var results []pipeline.TrackResult
	exit := 0
	for i := 0; i < f.tracks; i++ {
		track := uint32(i + 1)
		spec := synth.DefaultSpec(track, f.frames)
		spec.CorruptFrames = f.corrupt
		if i < f.truncate {
			spec.TruncateTail = true
		}
		// Vary each device's clock so the timeline repairs are exercised:
		// track 2 uses a coarse counter, track 3 has jitter, every third has a
		// rollback.
		// Devices differ: track 2 has coarse 10 ms precision, track 3 jitters
		// by a few capture units and jumps its clock backwards mid-shoot, which
		// the mapper must repair without breaking cadence.
		switch i % 3 {
		case 1:
			spec.Clock.CaptureUPS = 120
		case 2:
			spec.Clock.Jitter = 2
			spec.Clock.RollbackAt = f.frames / 2
			spec.Clock.RollbackBy = 10
		}
		paths := pipeline.Paths{Dir: work, Track: track}
		if _, err := synth.WriteSource(paths.Source(), spec, int64(1000+track)); err != nil {
			fmt.Fprintf(os.Stderr, "mediad: generate track %d: %v\n", track, err)
			return results, 2
		}

		opt := pipeline.Options{
			WorkDir: work, TrackID: track, TargetFrames: f.frames,
			RTPLoss: f.rtpLoss,
		}

		var (
			out *pipeline.TrackResult
			err error
		)
		if f.interrupt > 0 {
			// First run stops at the frame boundary and commits a checkpoint.
			opt.InterruptAfter = f.interrupt
			first, ferr := pipeline.Run(opt)
			if !errors.Is(ferr, pipeline.ErrInterrupted) {
				fmt.Fprintf(os.Stderr, "mediad: track %d interruption run: %v\n", track, ferr)
				if first != nil {
					results = append(results, *first)
				}
				return results, exitCode(ferr)
			}
			if f.partialIdx {
				if cerr := chopIndex(first.Paths.Index()); cerr != nil {
					fmt.Fprintf(os.Stderr, "mediad: track %d partial index: %v\n", track, cerr)
					return results, 3
				}
			}
			// Second run resumes from the verified checkpoint.
			opt.InterruptAfter = 0
			opt.Resume = true
			out, err = pipeline.Run(opt)
		} else {
			out, err = pipeline.Run(opt)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "mediad: track %d: %v\n", track, err)
			if out != nil {
				results = append(results, *out)
			}
			if exit == 0 {
				exit = exitCode(err)
			}
			continue
		}
		results = append(results, *out)
	}
	return results, exit
}

// chopIndex simulates an index writer killed mid-entry by removing the tail of
// the file, keeping the header and a whole number of entries.
func chopIndex(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	cut := fi.Size() - 13
	if cut < 16 {
		return fmt.Errorf("index too small to chop")
	}
	return os.Truncate(path, cut)
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	return model.ExitCode(err)
}

func printTable(results []pipeline.TrackResult) {
	fmt.Printf("%-6s %-9s %-9s %-13s %-14s %-11s %-11s %-17s %-12s %-16s %-7s\n",
		"track", "frames_ok", "frames_bad", "index_entries", "cache_verified",
		"target_kbps", "actual_kbps", "bitrate_delta_pct", "quality_mean",
		"manifest_skew_ms", "rtp_lost")
	for _, r := range results {
		m := r.Metrics
		fmt.Printf("%-6d %-9d %-9d %-13d %-14d %-11.1f %-11.1f %-17.2f %-12.3f %-16.3f %-7d\n",
			m.Track, m.FramesOK, m.FramesBad, m.IndexEntries, m.CacheVerified,
			m.TargetKbps, m.ActualKbps, m.BitrateDeltaPct, m.QualityMean,
			m.ManifestSkewMS, m.RTPLost)
	}
}
