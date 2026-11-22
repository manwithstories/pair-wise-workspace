package pipeline

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"archive/internal/checkpoint"
	"archive/internal/fault"
	"archive/internal/remux"
)

// Provenance is the per-segment record explaining where an output came
// from: the input digest, the track map applied, the track table with
// its time coverage, and the output digest held by the CAS.
type Provenance struct {
	Index          int               `json:"index"`
	Segment        string            `json:"segment"`
	ManifestSource string            `json:"manifest_source"`
	InputSHA256    string            `json:"input_sha256"`
	OutputSHA256   string            `json:"output_sha256"`
	MappingVersion string            `json:"mapping_version"`
	Tracks         []remux.TrackInfo `json:"tracks"`
	SyncMs         float64           `json:"sync_error_ms"`
	Bytes          int64             `json:"output_bytes"`
}

func provenanceDir(root string) string {
	return filepath.Join(root, "work", "provenance")
}

func provenancePath(root string, index int) string {
	// Nine digits, matching the checkpoint slots one-for-one.
	return filepath.Join(provenanceDir(root), fmt.Sprintf("seg-%09d.json", index))
}

// writeProvenance records the provenance atomically. It runs before the
// checkpoint commit, so a committed segment always has its explanation
// on disk; an orphan record for an uncommitted segment is harmless.
func writeProvenance(root string, p Provenance) error {
	dir := provenanceDir(root)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fault.Wrap(fault.Fatal, "pipeline", dir, err)
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return fault.Wrap(fault.Fatal, "pipeline", p.Segment, err)
	}
	tmp, err := os.CreateTemp(dir, "prov-*.tmp")
	if err != nil {
		return fault.Wrap(fault.Fatal, "pipeline", p.Segment, err)
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return fault.Wrap(fault.Fatal, "pipeline", p.Segment, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fault.Wrap(fault.Fatal, "pipeline", p.Segment, err)
	}
	if err := tmp.Close(); err != nil {
		return fault.Wrap(fault.Fatal, "pipeline", p.Segment, err)
	}
	if err := os.Rename(name, provenancePath(root, p.Index)); err != nil {
		return fault.Wrap(fault.Fatal, "pipeline", p.Segment, err)
	}
	return nil
}

// ReadProvenance reconstructs a verified segment's explanation and
// cross-checks it against the checkpoint that vouches for it.
func ReadProvenance(root string, e checkpoint.Entry) (Provenance, error) {
	raw, err := os.ReadFile(provenancePath(root, e.Index))
	if err != nil {
		return Provenance{}, fault.Wrap(fault.Fatal, "pipeline", e.URI, err)
	}
	var p Provenance
	if err := json.Unmarshal(raw, &p); err != nil {
		return Provenance{}, fault.Wrap(fault.Fatal, "pipeline", e.URI, err)
	}
	if p.OutputSHA256 != e.OutputSHA || p.InputSHA256 != e.InputSHA || p.Segment != e.URI {
		return Provenance{}, fault.Newf(fault.Fatal, "pipeline", e.URI,
			"provenance disagrees with checkpoint")
	}
	return p, nil
}

// ProvenanceLine renders the one-line source summary used in terminal
// output and stored in metrics.json.
func (p Provenance) String() string {
	return fmt.Sprintf("%s<-sha256:%s out:%s tracks=%d sync=%.1fms",
		p.Segment, short(p.InputSHA256), short(p.OutputSHA256), len(p.Tracks), p.SyncMs)
}

func short(d string) string {
	if len(d) > 12 {
		return d[:12]
	}
	return d
}
