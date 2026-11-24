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
	"reflect"
	"strconv"
	"strings"

	"media-check/internal/adapter"
	"media-check/internal/adapter/audio"
	"media-check/internal/adapter/frame"
	mp4adapter "media-check/internal/adapter/mp4"
	"media-check/internal/manifest"
	"media-check/internal/model"
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

// runDemo generates normal and damaged inputs locally, runs the batch
// pipeline repeat times over the same input, and prints per-segment
// hashes plus the acceptance metrics.
func runDemo(args []string) int {
	f, err := parseDemoFlags(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return exitUsage
	}
	assets, err := buildDemoAssets(f)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return exitKernel
	}

	p := newPipeline() // shared checkpoint store across repeats
	var runs []pipeline.AssetResult
	for r := 0; r < f.repeat; r++ {
		var round []pipeline.AssetResult
		for _, a := range assets {
			round = append(round, p.RunAsset(a))
		}
		if r > 0 && !reflect.DeepEqual(runs, round) {
			fmt.Println("recovery_stable=false")
			fmt.Fprintln(os.Stderr, "repeat run diverged; checkpoint recompute unstable")
			return exitKernel
		}
		runs = round
	}

	corrupt := 0
	keyHits, keyTotal := 0, 0
	rtOK, rtTotal := 0, 0
	hashMatch, hashTotal := 0, 0
	for _, res := range runs {
		corrupt += res.CorruptRanges
		keyHits += res.KeyHits
		keyTotal += res.KeyTotal
		rtOK += res.RoundtripOK
		rtTotal += res.RoundtripTotal
		for _, e := range res.Segments {
			printSegment(res.Name, e)
		}
		for _, d := range res.ColorDecisions {
			fmt.Printf("color_decision asset=%s segment=%d action=%s reason=%q\n",
				res.Name, d.Segment, d.Action, d.Reason)
		}
		for _, d := range res.AudioDecisions {
			fmt.Printf("audio_decision asset=%s segment=%d action=%s reason=%q\n",
				res.Name, d.Segment, d.Action, d.Reason)
		}
	}
	// segment hash match is measured over segments that entered the
	// source/target comparison, i.e. have a source hash at all.
	var zero [32]byte
	for _, res := range runs {
		for _, e := range res.Segments {
			if e.SrcHash == zero {
				continue // no source hash: nothing to compare
			}
			hashTotal++
			if e.Status == manifest.StatusOK && e.HashMatch() {
				hashMatch++
			}
		}
	}

	fmt.Printf("roundtrip_consistency=%.2f\n", ratio(rtOK, rtTotal))
	fmt.Printf("keyframe_hit_rate=%.2f\n", ratio(keyHits, keyTotal))
	fmt.Printf("segment_hash_match=%.2f\n", ratio(hashMatch, hashTotal))
	fmt.Printf("corrupt_ranges=%d\n", corrupt)
	fmt.Println("recovery_stable=true")

	if rtTotal > 0 && rtOK < rtTotal {
		return exitKernel // a valid segment failed roundtrip: kernel fault
	}
	if corrupt > 0 {
		return exitCorruption
	}
	return exitOK
}

func ratio(a, b int) float64 {
	if b == 0 {
		return 1.0
	}
	return float64(a) / float64(b)
}

func printSegment(asset string, e manifest.SegmentEntry) {
	fmt.Printf("segment asset=%s index=%d status=%s src_sha256=%x dst_sha256=%x range=[%d,%d) reason=%q\n",
		asset, e.Index, e.Status, e.SrcHash, e.DstHash, e.SrcRange.Start, e.SrcRange.End, e.Reason)
}

// buildDemoAssets creates the self-generated inputs: one frame-sequence
// asset (with optional timestamp wrap, hash corruption, colour tag gaps
// and an audio conflict) and, when requested, one truncated MP4 asset.
func buildDemoAssets(f demoFlags) ([]pipeline.Asset, error) {
	const (
		startSeq = uint32(1000)
		tsDelta  = uint32(3000) // 30 fps at 90 kHz
		ssrc     = uint32(0xA0C41E)
	)
	opts := frame.GenOptions{
		Count: f.frames, StartSeq: startSeq, StartTS: 90000, TSDelta: tsDelta,
		GOPSize: 30, Width: 1920, Height: 1080, PayloadSize: 64, Seed: 42,
		Color: model.ColorMeta{Primaries: "bt709", Transfer: "bt709", Matrix: "bt709"},
	}
	if f.inject["timestamp-wrap"] {
		opts.StartTS = 0xFFFF0000 // capture clock wraps ~22 frames in
	}
	recs := frame.Generate(opts)

	// Colour metadata boundary cases: some frames untagged, one segment
	// carries a conflicting specified tag.
	segFrames := f.segmentFrames
	for i := range recs {
		if i%segFrames == 20 {
			recs[i].Color = model.ColorMeta{}
		}
		if i == 5*segFrames+5 && i < len(recs) {
			recs[i].Color = model.ColorMeta{Primaries: "bt2020", Transfer: "pq", Matrix: "bt2020nc"}
		}
	}
	if f.inject["hash-corrupt"] && len(recs) > 3*segFrames+50 {
		// damage payloads after the supplier checksum was issued
		recs[3*segFrames+10].Payload[0] ^= 0xFF
		recs[3*segFrames+50].Payload[7] ^= 0xA5
	}

	packets, err := frame.ToPackets(recs, ssrc, 1)
	if err != nil {
		return nil, err
	}

	nSeg := (f.frames + segFrames - 1) / segFrames
	metas := make([]audio.Meta, nSeg)
	for i := range metas {
		metas[i] = audio.Meta{Channels: 2, Layout: "2.0", SampleRate: 48000}
	}
	if f.inject["audio-conflict"] && nSeg > 6 {
		metas[6] = audio.Meta{Channels: 6, Layout: "5.1", SampleRate: 44100}
	}

	assets := []pipeline.Asset{{
		Name: "clip-frames", Kind: "frames", Packets: packets, Audio: metas,
		StartSeq: startSeq, SegmentFrames: segFrames, TSDelta: tsDelta,
	}}

	if f.inject["box-truncate"] {
		full, err := mp4adapter.Adapter{}.Marshal(
			samplePayloads(24, 7), []uint32{1, 13}, model.RTPTimebase, tsDelta)
		if err != nil {
			return nil, err
		}
		truncated := full[:len(full)*3/5] // delivery cut mid-file
		assets = append(assets, pipeline.Asset{
			Name: "vendor-frag.mp4", Kind: "mp4", MP4Data: truncated,
		})
	}
	return assets, nil
}

func samplePayloads(n int, seed byte) [][]byte {
	out := make([][]byte, n)
	for i := range out {
		b := make([]byte, 48)
		for j := range b {
			b[j] = seed + byte(i)*31 + byte(j)*7
		}
		out[i] = b
	}
	return out
}
