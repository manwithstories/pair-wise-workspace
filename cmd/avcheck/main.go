// Command avcheck is the acceptance entry point. It generates the sample
// material, runs the staged pipeline cold, simulates an interruption, then
// resumes and verifies reproducibility, printing numeric evidence.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"avcheck/internal/clock"
	"avcheck/internal/fault"
	"avcheck/internal/pipeline"
	"avcheck/internal/render"
)

func main() { os.Exit(run(os.Args)) }

func run(argv []string) int {
	if len(argv) < 2 || argv[1] != "demo" {
		fmt.Fprintln(os.Stderr, "usage: avcheck demo --seed N --loss F --jitter-ms F --out DIR")
		return 2
	}
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	seed := fs.Int64("seed", 7301, "deterministic seed")
	loss := fs.Float64("loss", 0.08, "block loss probability")
	jitter := fs.Float64("jitter-ms", 35, "one-way jitter amplitude in ms")
	out := fs.String("out", "/tmp/avcheck-demo", "output directory")
	if err := fs.Parse(argv[2:]); err != nil {
		return 2
	}
	if *loss < 0 || *loss > 1 || *jitter < 0 || *seed < 0 {
		fmt.Fprintln(os.Stderr, "unsupported parameters")
		return 2
	}
	p := pipeline.Params{
		Seed: *seed, Loss: *loss, JitterMs: *jitter,
		OutDir: *out, FPS: 10, Width: 320, Height: 180,
	}

	// Cold run: normal + recoverable-corruption sample.
	a, err := pipeline.Run(p)
	if err != nil {
		fmt.Fprintln(os.Stderr, "run:", err)
		return fault.ExitCode(err)
	}

	// Simulate an interruption: the process "dies" after the mix stage, so
	// the render checkpoint and its frames are lost.
	os.Remove(filepath.Join(*out, "checkpoints", "checkpoint-render.json"))
	os.RemoveAll(filepath.Join(*out, "frames"))

	// Warm run: resumes from checkpoints; recover and mix must be cache hits.
	b, err := pipeline.Run(p)
	if err != nil {
		fmt.Fprintln(os.Stderr, "resume:", err)
		return fault.ExitCode(err)
	}

	syncMax := syncMaxMs(a)
	recovered := recoveredPct(a)
	p95 := p95Ms(a.Latencies)
	frameHash := framesDigest(a.Frames)
	resumeEqual := framesDigest(a.Frames) == framesDigest(b.Frames)

	for _, c := range a.Config.Conflicts {
		fmt.Println("conflict:", c)
	}
	fmt.Printf("sync_max_ms=%.4f\n", syncMax)
	fmt.Printf("recovered_pct=%.2f\n", recovered)
	fmt.Printf("p95_latency_ms=%.2f\n", p95)
	fmt.Printf("cache_hit=%d/3\n", b.CacheHits)
	fmt.Printf("resume_equal=%t\n", resumeEqual)
	fmt.Printf("frame_sha256=%s\n", frameHash)
	fmt.Printf("gaps=%d lost=%d recovered=%d duplicates=%d\n",
		len(a.Gaps), a.Stats.Lost, a.Stats.Recovered, a.Stats.Duplicates)

	if len(a.Gaps) > 0 {
		fmt.Fprintln(os.Stderr, "unrecovered corruption present")
		return 3
	}
	if syncMax > 0.10 || recovered < 90 || p95 > 180 || !resumeEqual {
		fmt.Fprintln(os.Stderr, "acceptance thresholds not met")
		return 1
	}
	return 0
}

// syncMaxMs measures the worst offset between nominal event onsets and the
// beep onsets detected in the mixed audio.
func syncMaxMs(r *pipeline.Result) float64 {
	const threshold = 8000
	frames := len(r.Mixed) / 2
	maxErr := 0.0
	for _, ev := range r.Events {
		nominal := clock.ToSamples(ev.Tick, 48000)
		lo := nominal - 240 // -5ms
		if lo < 0 {
			lo = 0
		}
		hi := nominal + 2400 // +50ms
		if hi > int64(frames) {
			hi = int64(frames)
		}
		detected := int64(-1)
		for i := lo; i < hi; i++ {
			l, rr := int(r.Mixed[i*2]), int(r.Mixed[i*2+1])
			if l < 0 {
				l = -l
			}
			if rr < 0 {
				rr = -rr
			}
			if l > threshold || rr > threshold {
				detected = i
				break
			}
		}
		if detected < 0 {
			return 999 // onset lost entirely
		}
		errMs := clock.ToMillis(clock.Tick(abs64(detected - nominal)))
		if errMs > maxErr {
			maxErr = errMs
		}
	}
	return maxErr
}

func recoveredPct(r *pipeline.Result) float64 {
	if r.Stats.Lost == 0 {
		return 100
	}
	return 100 * float64(r.Stats.Recovered) / float64(r.Stats.Lost)
}

func p95Ms(lat []float64) float64 {
	if len(lat) == 0 {
		return 0
	}
	s := append([]float64(nil), lat...)
	sort.Float64s(s)
	return s[int(0.95*float64(len(s)-1)+0.5)]
}

func framesDigest(frames []render.Frame) string {
	h := sha256.New()
	for _, f := range frames {
		h.Write([]byte(f.SHA256))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
