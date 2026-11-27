package gen

import (
	"encoding/json"
	"os"
	"path/filepath"

	"fieldcut/internal/ingest"
	"fieldcut/internal/media"
)

// Mutators are test/demo hooks that rewrite one manifest row after
// generation to exercise every rejection path without external files.
//
// They operate on manifest.json only; frame bytes are untouched.
func readManifest(dir string) (*ingest.Manifest, error) {
	data, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return nil, media.New(media.KindInput, "gen.manifest.read", "", -1, err)
	}
	var m ingest.Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, media.New(media.KindInput, "gen.manifest.read", "", -1, err)
	}
	return &m, nil
}

func writeManifest(dir string, m *ingest.Manifest) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return media.New(media.KindConflict, "gen.manifest.write", "", -1, err)
	}
	return os.WriteFile(filepath.Join(dir, "manifest.json"), append(data, '\n'), 0o644)
}
