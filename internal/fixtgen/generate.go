package fixtgen

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// ManifestEntry records one generated segment and its expected fault.
type ManifestEntry struct {
	ID       string `json:"id"`
	Index    int    `json:"index"`
	Fault    Fault  `json:"fault"`
	Source   string `json:"source"`
	Recoverable bool `json:"recoverable"`
}

// GenerateAll creates the full reproducible fixture batch under root.
// Layout:
//
//	root/sources/segNN/segNN.mp4 | .rtp
//	root/manifest.json
//
// Source files are never modified by the pipeline; outputs go elsewhere.
func GenerateAll(root string) ([]ManifestEntry, error) {
	srcRoot := filepath.Join(root, "sources")
	if err := os.MkdirAll(srcRoot, 0o755); err != nil {
		return nil, err
	}
	plan := Plan()
	entries := make([]ManifestEntry, 0, len(plan))
	for _, seg := range plan {
		dir := filepath.Join(srcRoot, seg.ID)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
		source := seg.ID + ".mp4"
		switch seg.Fault {
		case FaultRTPTruncated:
			p, err := WriteRTPSource(dir, seg)
			if err != nil {
				return nil, err
			}
			source = filepath.Base(p)
		case FaultIncompleteFrag:
			p, err := WriteFragmentedSource(dir, seg)
			if err != nil {
				return nil, err
			}
			source = filepath.Base(p)
		default:
			p, err := WriteBMFFSource(dir, seg)
			if err != nil {
				return nil, err
			}
			source = filepath.Base(p)
		}
		entries = append(entries, ManifestEntry{
			ID: seg.ID, Index: seg.Index, Fault: seg.Fault,
			Source:      filepath.Join(seg.ID, source),
			Recoverable: seg.Fault != FaultUnsupported,
		})
	}
	mf := filepath.Join(root, "manifest.json")
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(mf, data, 0o644); err != nil {
		return nil, err
	}
	return entries, nil
}
