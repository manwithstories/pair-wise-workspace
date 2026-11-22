// Package scan reads source manifests, parses MP4/MOV box layouts and edit
// lists, validates RTP timestamp sidecars and writes content-addressed,
// verifiable scan indexes.
package scan

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"mediaslice/media"
)

// ManifestVersion is the schema version of the source manifest.
const ManifestVersion = "mediaslice-source/v1"

// SourceEntry describes one bitrate representation on disk.
type SourceEntry struct {
	// ID is the stable representation identifier, e.g. "800k".
	ID string `json:"id"`
	// Bitrate is the nominal bitrate in bits per second.
	Bitrate int `json:"bitrate"`
	// MP4Path is relative to the manifest directory and must stay inside it.
	MP4Path string `json:"mp4"`
	// RTPPath optionally points to the concatenated RTP sidecar.
	RTPPath string `json:"rtp,omitempty"`
}

// Manifest is the input material list consumed by the scanner.
type Manifest struct {
	Version      string            `json:"version"`
	SegmentTicks media.Ticks       `json:"segment_ticks"`
	Sources      []SourceEntry     `json:"sources"`
	Params       map[string]string `json:"params,omitempty"`
}

// LoadManifest reads and validates a source manifest from path.
func LoadManifest(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, media.NewError(media.KindIO, path, err.Error())
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("manifest %s: invalid JSON: %w", path, err)
	}
	if m.Version != ManifestVersion {
		return nil, fmt.Errorf("manifest %s: unsupported version %q", path, m.Version)
	}
	if len(m.Sources) == 0 {
		return nil, fmt.Errorf("manifest %s: no sources", path)
	}
	root := filepath.Dir(filepath.Clean(path))
	seen := map[string]bool{}
	for i := range m.Sources {
		e := &m.Sources[i]
		if e.ID == "" || seen[e.ID] {
			return nil, fmt.Errorf("manifest %s: duplicate or empty source id", path)
		}
		seen[e.ID] = true
		if e.MP4Path == "" {
			return nil, fmt.Errorf("manifest %s: source %s missing mp4 path", path, e.ID)
		}
		mp4Abs, err := SafeJoin(root, e.MP4Path)
		if err != nil {
			return nil, fmt.Errorf("manifest %s: %w", path, err)
		}
		e.MP4Path = mp4Abs
		if e.RTPPath != "" {
			rtpAbs, err := SafeJoin(root, e.RTPPath)
			if err != nil {
				return nil, fmt.Errorf("manifest %s: %w", path, err)
			}
			e.RTPPath = rtpAbs
		}
	}
	return &m, nil
}

// SafeJoin resolves name below root and rejects any path that escapes it.
// All pipeline output and input resolution goes through this guard so a
// crafted manifest can never touch files outside the working directory.
func SafeJoin(root, name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("empty path")
	}
	if filepath.IsAbs(name) {
		return "", fmt.Errorf("absolute paths are not allowed: %s", name)
	}
	cleanRoot := filepath.Clean(root)
	clean := filepath.Clean(filepath.Join(cleanRoot, name))
	rel, err := filepath.Rel(cleanRoot, clean)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes working directory", name)
	}
	return clean, nil
}
