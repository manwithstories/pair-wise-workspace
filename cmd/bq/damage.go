package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"

	"bq/internal/fixture"
	"bq/internal/pipeline"
)

// writeDamageSamples produces one example per required damage case beneath
// outDir/damage. Each sample is independently verifiable with `bq verify`.
func writeDamageSamples(ctx context.Context, outDir string, base fixture.Options) error {
	dmgDir := filepath.Join(outDir, "damage")
	if err := os.MkdirAll(dmgDir, 0o755); err != nil {
		return err
	}

	// Case 1: corrupt manifest (truncated JSON).
	if err := writeCorruptManifest(base, filepath.Join(dmgDir, "corrupt-manifest.json")); err != nil {
		return err
	}

	// Cases 2-4 share a generated run but toggle one anomaly each.
	cases := []struct {
		name string
		opts func(fixture.Options) fixture.Options
		file string
	}{
		{"pts-wrap", func(o fixture.Options) fixture.Options {
			o.InjectWrap, o.InjectDup, o.InjectTrunc = true, false, false
			return o
		}, "pts-wrap.json"},
		{"duplicate", func(o fixture.Options) fixture.Options {
			o.InjectWrap, o.InjectDup, o.InjectTrunc = false, true, false
			return o
		}, "duplicate.json"},
		{"tail-truncation", func(o fixture.Options) fixture.Options {
			o.InjectWrap, o.InjectDup, o.InjectTrunc = false, false, true
			return o
		}, "tail-truncation.json"},
	}
	for _, c := range cases {
		opts := c.opts(base)
		caseDir := filepath.Join(dmgDir, c.name)
		m, err := pipeline.Generate(ctx, opts, caseDir)
		if err != nil {
			return err
		}
		// Re-write a top-level alias manifest pointing at the case directory.
		if err := fixture.WriteManifest(m, filepath.Join(dmgDir, c.file)); err != nil {
			return err
		}
	}
	return nil
}

// writeCorruptManifest writes syntactically broken JSON with a recognizable
// suffix; strict parsing must reject it with a byte-offset record.
func writeCorruptManifest(base fixture.Options, path string) error {
	m := fixture.Build(base)
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	// Cut the document well inside the packet array so the offset is real.
	cut := len(data) * 3 / 4
	if cut > len(data) {
		cut = len(data)
	}
	return os.WriteFile(path, data[:cut], 0o644)
}
