package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"mediapack/internal/fault"
	"mediapack/internal/fixture"
	"mediapack/internal/index"
	"mediapack/internal/pipeline"
)

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// validateIndex parses the generated index against the actual stream size.
func validateIndex(b *generatedBundle) ([]index.Entry, []index.Report, error) {
	return index.Parse(b.indexBytes, int64(len(b.stream)))
}

func durationMs(ms int) time.Duration { return time.Duration(ms) * time.Millisecond }

func runVerify(args []string) int {
	f, err := parseFlags(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 4
	}
	// verify regenerates the exact canonical 1 MiB sample used by demo.
	bundle, err := generateSample(f.seed, 1<<20)
	if err != nil {
		return emitFault(err)
	}
	if _, _, err := validateIndex(bundle); err != nil {
		return emitFault(err)
	}
	workDir := filepath.Join(os.TempDir(), "mediapack-work", fmt.Sprintf("seed-%d", f.seed))
	cfg := pipeline.Config{
		SourceID:   fmt.Sprintf("seed-%d/h264", f.seed),
		ChunkBytes: 4096,
		WorkDir:    workDir,
	}
	// A verify run reuses the cache produced by demo and revalidates every
	// entry before trusting it.
	p, err := pipeline.New(cfg)
	if err != nil {
		return emitFault(err)
	}
	res, err := p.Run(context.Background(), bundle.stream)
	if err != nil {
		return emitFault(err)
	}
	// Independently recompute the source digest.
	expectSource := sha256Hex(bundle.stream)
	if expectSource != res.Metrics.SourceSHA256 {
		fe := fault.New(fault.KindChecksum, "verify", "source digest mismatch")
		return emitFault(fe)
	}
	out := map[string]interface{}{
		"output_sha256":     res.Metrics.OutputSHA256,
		"source_sha256":     res.Metrics.SourceSHA256,
		"cache_revalidated": res.Metrics.CacheRevalidated,
		"packets":           res.Metrics.Packets,
		"verify":            "ok",
	}
	raw, _ := json.MarshalIndent(out, "", "  ")
	fmt.Println(string(raw))
	return 0
}

var _ = fixture.BadIndexMagic
