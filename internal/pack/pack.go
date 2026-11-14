// Package pack writes manifest.mpd and index.json from a fixture root,
// reusing the mpd builder and template expander.
package pack

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"dashpack/internal/align"
	"dashpack/internal/mp4"
	"dashpack/internal/mpd"
)

// FixtureIndex is the fixtures.json descriptor written by the generator.
type FixtureIndex struct {
	Representations []align.RepMeta `json:"representations"`
}

// Index is the index.json summary written next to manifest.mpd.
type Index struct {
	GeneratedBy     string            `json:"generated_by"`
	Report          *align.Report     `json:"report"`
	Representations []align.RepResult `json:"representations"`
	Files           []string          `json:"files"`
}

// LoadMetas reads fixtures.json from a fixture root.
func LoadMetas(root string) ([]align.RepMeta, error) {
	data, err := os.ReadFile(filepath.Join(root, "fixtures.json"))
	if err != nil {
		return nil, fmt.Errorf("pack: read fixtures.json: %w", err)
	}
	var fx FixtureIndex
	if err := json.Unmarshal(data, &fx); err != nil {
		return nil, fmt.Errorf("pack: parse fixtures.json: %v", err)
	}
	if len(fx.Representations) == 0 {
		return nil, fmt.Errorf("pack: fixtures.json lists no representations")
	}
	return fx.Representations, nil
}

// ScanAll scans every representation directory under root.
func ScanAll(root string, metas []align.RepMeta) (map[string]*mp4.Timeline, error) {
	tls := make(map[string]*mp4.Timeline, len(metas))
	for _, m := range metas {
		tl, err := mp4.ScanRepresentation(filepath.Join(root, m.Dir))
		if err != nil {
			return nil, err
		}
		tls[m.ID] = tl
	}
	return tls, nil
}

// ExpandAll expands the media template for every filled segment, counting
// successes and failures. The init template is expanded once per
// representation as a validity check but is not counted.
func ExpandAll(metas []align.RepMeta, tls map[string]*mp4.Timeline) (expanded, failed int, err error) {
	for _, m := range metas {
		tl := tls[m.ID]
		if tl == nil {
			return expanded, failed, fmt.Errorf("pack: no timeline for %q", m.ID)
		}
		vars := mpd.Vars{RepresentationID: m.ID, Bandwidth: m.Bandwidth}
		if _, err := mpd.Expand(mpd.InitTemplate, vars); err != nil {
			return expanded, failed, fmt.Errorf("pack: init template for %q: %v", m.ID, err)
		}
		for _, seg := range tl.FilledSegments() {
			vars.Number = uint64(seg.Number)
			vars.Time = seg.Start
			if _, err := mpd.Expand(mpd.MediaTemplate, vars); err != nil {
				failed++
			} else {
				expanded++
			}
		}
	}
	return expanded, failed, nil
}

// Pack scans the fixture root, aligns timelines, builds the manifest and
// writes manifest.mpd plus index.json atomically. Nothing is written when
// any earlier step fails.
func Pack(root, outDir string, thresholdMs float64) (*align.Report, error) {
	metas, err := LoadMetas(root)
	if err != nil {
		return nil, err
	}
	tls, err := ScanAll(root, metas)
	if err != nil {
		return nil, err
	}
	al, err := align.Align(metas, tls, thresholdMs)
	if err != nil {
		return nil, err
	}
	manifest, err := mpd.Build(metas, tls, al)
	if err != nil {
		return nil, err
	}
	expanded, failed, err := ExpandAll(metas, tls)
	if err != nil {
		return nil, err
	}
	if failed > 0 {
		return nil, fmt.Errorf("pack: %d template expansions failed, refusing to write manifest", failed)
	}
	report := align.BuildReport(metas, tls, al, expanded, failed)
	body, err := xml.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("pack: marshal manifest: %v", err)
	}
	mpdBytes := append([]byte(xml.Header), body...)
	mpdBytes = append(mpdBytes, '\n')
	idx := &Index{
		GeneratedBy:     "dashpack",
		Report:          report,
		Representations: al.Reps,
		Files:           []string{"manifest.mpd", "index.json"},
	}
	idxBytes, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("pack: marshal index: %v", err)
	}
	idxBytes = append(idxBytes, '\n')
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, fmt.Errorf("pack: create output dir: %w", err)
	}
	if err := writeAtomic(filepath.Join(outDir, "manifest.mpd"), mpdBytes); err != nil {
		return nil, err
	}
	if err := writeAtomic(filepath.Join(outDir, "index.json"), idxBytes); err != nil {
		return nil, err
	}
	return report, nil
}

// writeAtomic writes via a temp file plus rename so partial output never
// appears at the target path.
func writeAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("pack: write %s: %w", path, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("pack: rename %s: %w", path, err)
	}
	return nil
}

// SortedMetas returns metas sorted by bandwidth then ID for stable output.
func SortedMetas(metas []align.RepMeta) []align.RepMeta {
	out := make([]align.RepMeta, len(metas))
	copy(out, metas)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Bandwidth != out[j].Bandwidth {
			return out[i].Bandwidth < out[j].Bandwidth
		}
		return out[i].ID < out[j].ID
	})
	return out
}
