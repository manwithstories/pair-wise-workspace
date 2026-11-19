package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"mediapipeline/internal/index"
	"mediapipeline/internal/mediaerr"
	"mediapipeline/internal/metrics"
	"mediapipeline/internal/pipeline"
	"mediapipeline/internal/testdata"
)

// printLocatedFailures lists the structured errors collected for isolated
// faults so each one is traceable to a stage, track and sequence number.
func printLocatedFailures(res *pipeline.Result) {
	shown := 0
	for _, e := range res.Failures {
		if shown >= 8 {
			fmt.Printf("... %d more isolated failures omitted\n", len(res.Failures)-shown)
			break
		}
		fmt.Printf("isolated_failure=%s\n", e.Error())
		shown++
	}
}

func parseVerifyFlags(args []string) (int64, error) {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	seed := fs.Int64("seed", 42, "deterministic fixture seed")
	if err := fs.Parse(args); err != nil {
		return 0, err
	}
	return *seed, nil
}

func runVerify(args []string) int {
	seed, err := parseVerifyFlags(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid arguments: %v\n", err)
		return 2
	}
	failures := 0

	// 1. Reproducibility: identical seed must produce identical segment hashes.
	failures += checkReproducibility(seed)
	// 2. Round-trip and keyframe hit rates on the clean baseline fixture.
	failures += checkBaseline(seed)
	// 3. Boundary fixtures: wrap, empty, no-keyframe.
	failures += checkBoundaries(seed)
	// 4. Slow-consumer timeout must surface as a cancellation-class error.
	failures += checkSlowConsumer(seed)
	// 5. Truncated packet and out-of-range index must be located.
	failures += checkFaultLocation(seed)

	fmt.Printf("verify_failures=%d\n", failures)
	if failures > 0 {
		return 1
	}
	fmt.Println("verify=passed")
	return 0
}

func buildRunner() (*pipeline.Runner, *mediaerr.Error) {
	cfg := pipeline.Config{
		MaxBufferBytes: 1 << 20, MaxBufferPackets: 256,
		ReorderWindow: 64, ConsumerTimeoutNS: 5_000_000_000,
	}
	opts := pipeline.RunOptions{SegmentNS: 500_000_000, Audio: pipeline.DefaultAudioSpec()}
	return pipeline.NewRunner(cfg, opts)
}

func checkReproducibility(seed int64) int {
	spec := testdata.DefaultSpec(seed)
	r1, merr := buildRunner()
	if merr != nil {
		fmt.Println("reproducibility: runner build failed:", merr)
		return 1
	}
	res1, merr := r1.Run(context.Background(), testdata.Generate(spec))
	if merr != nil {
		fmt.Println("reproducibility: first run failed:", merr)
		return 1
	}
	r2, _ := buildRunner()
	res2, merr := r2.Run(context.Background(), testdata.Generate(spec))
	if merr != nil {
		fmt.Println("reproducibility: second run failed:", merr)
		return 1
	}
	if len(res1.Segments) != len(res2.Segments) {
		fmt.Println("reproducibility: segment count differs")
		return 1
	}
	for i := range res1.Segments {
		if res1.Segments[i].SHA256 != res2.Segments[i].SHA256 {
			fmt.Printf("reproducibility: segment %d hash differs for fixed seed\n", i)
			return 1
		}
	}
	fmt.Printf("reproducibility=passed segments=%d\n", len(res1.Segments))
	return 0
}

func checkBaseline(seed int64) int {
	runner, _ := buildRunner()
	fx := testdata.Generate(testdata.DefaultSpec(seed))
	res, merr := runner.Run(context.Background(), fx)
	if merr != nil {
		fmt.Println("baseline: run failed:", merr)
		return 1
	}
	summary := metrics.Evaluate(res, fx.MediaDatagrams(), metrics.DefaultThresholds())
	metrics.WriteText(os.Stdout, summary)
	if !summary.Passed {
		return 1
	}
	return 0
}

func checkBoundaries(seed int64) int {
	fails := 0
	specs := testdata.BoundarySpecs(seed)
	for name, spec := range specs {
		if name == "slow_consumer" {
			continue
		}
		runner, _ := buildRunner()
		res, merr := runner.Run(context.Background(), testdata.Generate(spec))
		if merr != nil {
			fmt.Printf("boundary[%s]: unexpected fatal: %v\n", name, merr)
			fails++
			continue
		}
		fmt.Printf("boundary[%s]=handled segments=%d parsed=%d\n",
			name, len(res.Segments), res.Counters.Parsed)
	}
	return fails
}

func checkSlowConsumer(seed int64) int {
	cfg := pipeline.Config{
		MaxBufferBytes: 1 << 16, MaxBufferPackets: 4,
		ReorderWindow: 4, ConsumerTimeoutNS: int64(5 * time.Millisecond),
	}
	opts := pipeline.RunOptions{
		SegmentNS: 500_000_000, ConsumerDelayNS: int64(50 * time.Millisecond),
		Audio: pipeline.DefaultAudioSpec(),
	}
	runner, merr := pipeline.NewRunner(cfg, opts)
	if merr != nil {
		fmt.Println("slow_consumer: build failed:", merr)
		return 1
	}
	_, merr = runner.Run(context.Background(), testdata.Generate(testdata.DefaultSpec(seed)))
	if merr == nil {
		fmt.Println("slow_consumer: expected timeout, got success")
		return 1
	}
	if merr.Kind != mediaerr.KindTimeout && merr.Kind != mediaerr.KindCanceled {
		fmt.Printf("slow_consumer: wrong error kind %v\n", merr.Kind)
		return 1
	}
	fmt.Printf("slow_consumer=timed_out kind=%s\n", merr.Kind)
	return 0
}

func checkFaultLocation(seed int64) int {
	fails := 0
	// Truncated packet is located by the parse stage.
	spec := testdata.DefaultSpec(seed)
	spec.AudioPkts = 16
	spec.VideoPkts = 4
	spec.Corruptions = []testdata.Corruption{
		{Track: spec.AudioSSRC, SeqIndex: 3, Kind: testdata.CorruptTruncated},
	}
	runner, _ := buildRunner()
	res, merr := runner.Run(context.Background(), testdata.Generate(spec))
	if merr != nil {
		fmt.Println("fault_location: run failed:", merr)
		return 1
	}
	if res.Counters.ParseErrors == 0 {
		fmt.Println("fault_location: truncated packet was not located")
		fails++
	} else {
		fmt.Printf("fault_location[truncated]=located parse_errors=%d\n", res.Counters.ParseErrors)
	}
	// Out-of-range keyframe index must be rejected, never yielding a false hit.
	idx, merr := index.Build(2, []index.Sample{
		{IsKeyframe: true, Size: 4}, {IsKeyframe: false, Size: 4},
	})
	if merr != nil {
		fmt.Println("fault_location: index build failed:", merr)
		return 1
	}
	bogus := []index.Entry{{SampleNumber: 99, Offset: 0}}
	if merr := idx.Validate(bogus); merr == nil || merr.Kind != mediaerr.KindIndexOutOfRange {
		fmt.Printf("fault_location: out-of-range index not rejected: %v\n", merr)
		fails++
	} else {
		fmt.Printf("fault_location[index_out_of_range]=rejected %s\n", merr.Error())
	}
	return fails
}
