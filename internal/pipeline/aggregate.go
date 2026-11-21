package pipeline

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"

	"mediad/internal/audio"
	"mediad/internal/fingerprint"
	"mediad/internal/segment"
	"mediad/internal/verify"
)

// fingerprintFile streams a fragment through the deterministic RTP
// fingerprint, keeping peak memory independent of fragment size.
func fingerprintFile(path string, truncated bool) (*fingerprint.Fingerprint, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	src := fingerprint.Source{URI: filepath.Base(path)}
	return fingerprint.FingerprintReader(f, src, truncated)
}

// digestStrings folds an ordered list of digests into one stable hash.
func digestStrings(items []string) string {
	h := sha256.New()
	for _, s := range items {
		h.Write([]byte(s))
		h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// runAggregate converges every leaf artifact: it collects cuts, silence
// intervals, fingerprints (for cross-track dedup) and bad offsets, then
// persists a single deterministic aggregate artifact.
func (e *Engine) runAggregate() error {
	artifacts, err := e.readArtifacts()
	if err != nil {
		return err
	}
	var (
		cuts    []segment.Cut
		silence []audio.Interval
		fps     []*fingerprint.Fingerprint
		bad     []verify.BadOffset
	)
	for i := range artifacts {
		a := artifacts[i]
		if a.Cut != nil {
			cuts = append(cuts, *a.Cut)
		}
		silence = append(silence, a.Silence...)
		if a.FP != nil {
			// Re-attach provenance before dedup so every source is retained.
			a.FP.Source = fingerprint.Source{
				Track: a.Segment.Track, Index: a.Segment.Index, URI: a.Segment.URI,
			}
			fps = append(fps, a.FP)
		}
		if a.Bad != nil {
			bad = append(bad, *a.Bad)
		}
	}
	dups := fingerprint.FindDuplicates(fps)
	agg := aggregateArtifact{Cuts: cuts, Silence: silence, Duplicates: dups, Bad: bad}
	sort.Slice(agg.Bad, func(i, j int) bool {
		if agg.Bad[i].Offset != agg.Bad[j].Offset {
			return agg.Bad[i].Offset < agg.Bad[j].Offset
		}
		return agg.Bad[i].Track < agg.Bad[j].Track
	})
	data, err := json.MarshalIndent(&agg, "", "  ")
	if err != nil {
		return err
	}
	final := filepath.Join(e.outDir, "artifacts", "aggregate.json")
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, final)
}

// readAggregate loads the convergence artifact produced by runAggregate.
func (e *Engine) readAggregate() (*aggregateArtifact, error) {
	data, err := os.ReadFile(filepath.Join(e.outDir, "artifacts", "aggregate.json"))
	if err != nil {
		return nil, err
	}
	var agg aggregateArtifact
	if err := json.Unmarshal(data, &agg); err != nil {
		return nil, err
	}
	return &agg, nil
}
