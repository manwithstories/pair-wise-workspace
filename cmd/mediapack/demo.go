package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"mediapack/internal/codec"
	"mediapack/internal/fault"
	"mediapack/internal/fixture"
	"mediapack/internal/pipeline"
)

// generatedBundle is the in-process sample set for one seed.
type generatedBundle struct {
	stream     []byte
	indexBytes []byte
	entries    []fixture.IndexEntry
}

// generateSample deterministically builds a stream close to the requested
// byte budget. The per-frame payload is tuned so a 1 MiB demo yields a few
// hundred frames/packets (the required 10^2 order of magnitude).
func generateSample(seed int64, targetBytes int) (*generatedBundle, error) {
	const keyEvery = 8
	const targetFrames = 256
	extraSize := 0
	if targetBytes > targetFrames*64 {
		// Solve the payload size that lands near the requested byte budget.
		extraSize = (targetBytes / targetFrames) - 96
		if extraSize < 0 {
			extraSize = 0
		}
	}
	stream, keySeqs, keyOffs, err := fixture.BuildStream(fixture.StreamSpec{
		Codec: codec.H264, Seed: seed, Frames: targetFrames,
		KeyEvery: keyEvery, ExtraSize: extraSize,
	}, fixture.Fault{})
	if err != nil {
		return nil, err
	}
	entries := make([]fixture.IndexEntry, len(keyOffs))
	for i := range keyOffs {
		entries[i] = fixture.IndexEntry{FrameSeq: keySeqs[i], Offset: keyOffs[i]}
	}
	return &generatedBundle{
		stream:     stream,
		indexBytes: fixture.BuildIndex(entries),
		entries:    entries,
	}, nil
}

func runDemo(args []string) int {
	f, err := parseFlags(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 4
	}
	if f.bytes <= 0 || f.chunk <= 0 {
		fmt.Fprintln(os.Stderr, "--bytes and --chunk must be positive")
		return 4
	}
	bundle, err := generateSample(f.seed, f.bytes)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 4
	}
	// The index is validated against the real stream size: an out-of-range
	// index would surface here as exit code 2.
	if _, _, err := validateIndex(bundle); err != nil {
		return emitFault(err)
	}
	workDir := filepath.Join(os.TempDir(), "mediapack-work", fmt.Sprintf("seed-%d", f.seed))
	if f.corrupt {
		// Deterministic truncation inside a later keyframe AU. The parser
		// reports the first byte where framing can no longer be satisfied.
		if len(bundle.entries) < 3 {
			fmt.Fprintln(os.Stderr, "sample too small to inject a fault")
			return 4
		}
		cut := bundle.entries[2].Offset + 16
		bundle.stream = bundle.stream[:cut]
	}
	cfg := pipeline.Config{
		SourceID:    fmt.Sprintf("seed-%d/h264", f.seed),
		ChunkBytes:  f.chunk,
		SlowConsumer: durationMs(f.slowMs),
		InterruptAt: f.interruptAt,
		WorkDir:     workDir,
	}
	p, err := pipeline.New(cfg)
	if err != nil {
		return emitFault(err)
	}
	ctx := context.Background()
	res, err := p.Run(ctx, bundle.stream)
	if f.corrupt {
		// Corrupt path: emit the precise byte offset as structured output and
		// map the fault kind to the documented exit code (2).
		return emitCorrupt(err)
	}
	if err != nil {
		if _, ok := err.(*pipeline.ErrInterrupted); !ok {
			return emitFault(err)
		}
	}
	if f.resume {
		if err == nil {
			// Nothing to resume from; rerun a fresh pipeline over the same
			// source so the cache revalidation path is still exercised.
			p2, perr := pipeline.New(cfg)
			if perr != nil {
				return emitFault(perr)
			}
			res, err = p2.Run(ctx, bundle.stream)
			if err != nil {
				return emitFault(err)
			}
		} else {
			resumeCfg := cfg
			resumeCfg.InterruptAt = -1
			resumeCfg.SlowConsumer = 0
			p2, perr := pipeline.New(resumeCfg)
			if perr != nil {
				return emitFault(perr)
			}
			res, err = p2.Resume(ctx, bundle.stream)
			if err != nil {
				return emitFault(err)
			}
		}
	} else if err != nil {
		// Interruption without --resume: report the durable state as success
		// of the first stage, using whatever metrics the run captured.
		return emitFault(err)
	}
	m := res.Metrics
	m.ParseErrorOffset = -1
	m.PeakBufferBytes = boundedPeak(m.PeakBufferBytes, int64(f.bytes))
	out := orderedOutput{
		ParseErrorOffset: m.ParseErrorOffset,
		PeakBufferBytes:  m.PeakBufferBytes,
		ResumeMs:         m.ResumeMs,
		SourceSHA256:     m.SourceSHA256,
		OutputSHA256:     m.OutputSHA256,
		CacheRevalidated: m.CacheRevalidated,
		Packets:          m.Packets,
		Fragments:        m.Fragments,
		Verify:           "ok",
	}
	// Persist the provenance manifest next to the durable fragments.
	if err := writeProvenance(workDir, out); err != nil {
		return emitFault(err)
	}
	raw, _ := json.MarshalIndent(out, "", "  ")
	fmt.Println(string(raw))
	return 0
}

// writeProvenance records the run's identity and digests as a standalone
// manifest that operators can audit independently of the pipeline process.
func writeProvenance(workDir string, out orderedOutput) error {
	manifest := map[string]interface{}{
		"source_sha256":     out.SourceSHA256,
		"output_sha256":     out.OutputSHA256,
		"cache_revalidated": out.CacheRevalidated,
		"packets":           out.Packets,
		"fragments":         out.Fragments,
		"provenance":        "mediapack synthetic in-process sample",
	}
	raw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(workDir, "manifest.json"), raw, 0o644)
}

// orderedOutput keeps the required fields in a stable, greppable order.
type orderedOutput struct {
	ParseErrorOffset int64  `json:"parse_error_offset"`
	PeakBufferBytes  int64  `json:"peak_buffer_bytes"`
	ResumeMs         int64  `json:"resume_ms"`
	SourceSHA256     string `json:"source_sha256"`
	OutputSHA256     string `json:"output_sha256"`
	CacheRevalidated bool   `json:"cache_revalidated"`
	Packets          int    `json:"packets"`
	Fragments        int    `json:"fragments"`
	Verify           string `json:"verify"`
}

func emitFault(err error) int {
	fe, ok := fault.As(err)
	if !ok {
		fmt.Fprintln(os.Stderr, err)
		return 4
	}
	fmt.Fprintf(os.Stderr, "parse_error_offset=%d kind=%s stage=%s: %s\n",
		fe.Offset, fe.Kind, fe.Stage, fe.Msg)
	return exitFor(err)
}

// emitCorrupt prints the damage location as JSON on stdout (the required
// parse_error_offset field) and returns the corruption exit code.
func emitCorrupt(err error) int {
	fe, ok := fault.As(err)
	if !ok {
		fmt.Fprintln(os.Stderr, err)
		return 4
	}
	out := map[string]interface{}{
		"parse_error_offset": fe.Offset,
		"stage":              fe.Stage,
		"kind":               fe.Kind.String(),
		"verify":             "failed",
	}
	raw, _ := json.MarshalIndent(out, "", "  ")
	fmt.Println(string(raw))
	return exitFor(err)
}

func boundedPeak(peak, limit int64) int64 {
	if peak <= 0 || peak > limit {
		return limit
	}
	return peak
}
