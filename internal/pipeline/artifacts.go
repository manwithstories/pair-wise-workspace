package pipeline

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"relayctl/internal/cmaf"
)

// fragmentOutFile is the deterministic per-fragment normalized artifact name.
func fragmentOutFile(dir string, seq uint32) string {
	return filepath.Join(dir, fmt.Sprintf("fragment-%05d.m4s", seq))
}

// writeArtifacts materializes each validated fragment as one normalized file
// plus a small index, all under a single local output directory. Bytes are
// the recovered stream slices, so identical runs produce identical hashes.
func writeArtifacts(dir string, e *Engine) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("pipeline: create output dir: %w", err)
	}
	var index []string
	for _, f := range e.fragments {
		name := fragmentOutFile(dir, f.SequenceNumber)
		slice := e.stream()[f.Start:f.End]
		if err := os.WriteFile(name, slice, 0o644); err != nil {
			return fmt.Errorf("pipeline: write fragment: %w", err)
		}
		index = append(index, filepath.Base(name))
	}
	idxPath := filepath.Join(dir, "index.txt")
	var idx []byte
	for _, line := range index {
		idx = append(idx, line...)
		idx = append(idx, '\n')
	}
	if err := os.WriteFile(idxPath, idx, 0o644); err != nil {
		return fmt.Errorf("pipeline: write index: %w", err)
	}
	return nil
}

// fragmentCount is a small helper used by status rendering.
func fragmentCount(frags []*cmaf.Fragment) int { return len(frags) }

func itoa64(v int64) string { return strconv.FormatInt(v, 10) }
