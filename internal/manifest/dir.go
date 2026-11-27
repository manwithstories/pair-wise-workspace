package manifest

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"avrepair/internal/media"
)

// Document names recognized inside a case directory. A directory may
// contain an HLS presentation and a DASH presentation simultaneously;
// their entries are merged into one timeline, which is how an HLS
// discontinuity track and a DASH timescale track are combined.
const (
	HLSIndexName = "index.m3u8"
	DASHName     = "presentation.mpd"
)

// LoadDir reads a case directory and merges every recognized manifest.
// Track ids are kept unique by suffixing collisions with the source kind.
func LoadDir(dir string) (*Manifest, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("manifest: %s is not a directory", dir)
	}
	var sources []*Manifest
	if p := filepath.Join(dir, HLSIndexName); fileExists(p) {
		raw, err := media.ReadManifest(p)
		if err != nil {
			return nil, err
		}
		m, err := ParseHLS(p, raw)
		if err != nil {
			return nil, err
		}
		sources = append(sources, m)
	}
	if p := filepath.Join(dir, DASHName); fileExists(p) {
		raw, err := media.ReadManifest(p)
		if err != nil {
			return nil, err
		}
		m, err := ParseDASH(p, raw)
		if err != nil {
			return nil, err
		}
		sources = append(sources, m)
	}
	if len(sources) == 0 {
		if m, err := loadSingle(dir); err != nil {
			return nil, err
		} else {
			return m, nil
		}
	}
	merged := &Manifest{Kind: "merged", Source: filepath.Clean(dir)}
	for _, m := range sources {
		merged.Kind = mergeKind(merged.Kind, m.Kind)
		merged.Warnings = append(merged.Warnings, m.Warnings...)
		if merged.Timescale == 0 {
			merged.Timescale = m.Timescale
		}
		merged.Entries = append(merged.Entries, m.Entries...)
	}
	resolveTrackCollisions(merged)
	if len(merged.Entries) == 0 {
		return nil, &media.FaultError{Kind: media.ErrNoManifest, Detail: dir}
	}
	return merged, nil
}

func mergeKind(a, b string) string {
	if a == "merged" || b == "merged" {
		return "merged"
	}
	if a == b {
		return a
	}
	return "merged"
}

// resolveTrackCollisions renames a track id only when two different
// sources claim the same id. Entries from one source keep their id.
func resolveTrackCollisions(m *Manifest) {
	owner := map[string]string{}
	for i := range m.Entries {
		id, src := m.Entries[i].Track, m.Entries[i].Source
		if prev, ok := owner[id]; ok && prev != src {
			m.Entries[i].Track = id + "-" + src
			continue
		}
		owner[id] = src
	}
}

// loadSingle finds exactly one manifest file in dir.
func loadSingle(dir string) (*Manifest, error) {
	names := []string{"index.m3u8", "presentation.mpd", "manifest.mpd", "playlist.m3u8", "stream.m3u8", "stream.mpd"}
	for _, n := range names {
		p := filepath.Join(dir, n)
		if fileExists(p) {
			return Load(p)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var found []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		ext := filepath.Ext(e.Name())
		if ext == ".m3u8" || ext == ".mpd" {
			found = append(found, e.Name())
		}
	}
	if len(found) != 1 {
		return nil, &media.FaultError{Kind: media.ErrNoManifest, Detail: fmt.Sprintf("%s has %d manifests", dir, len(found))}
	}
	sort.Strings(found)
	return Load(filepath.Join(dir, found[0]))
}

func fileExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.Mode().IsRegular()
}
