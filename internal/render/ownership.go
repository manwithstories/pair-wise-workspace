package render

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"fieldcut/internal/media"
)

// refuseForeign rejects entries in the output root or frame directory that
// fieldcut cannot prove it wrote: unknown root entries and malformed frame
// names are never overwritten.
func refuseForeign(dir string) error {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return media.New(media.KindConflict, "export.open", "", -1, err)
	}
	for _, e := range ents {
		if e.Name() == "frames" && e.IsDir() {
			continue
		}
		if e.Name() == ProgressName && !e.IsDir() {
			if data, err := os.ReadFile(filepath.Join(dir, ProgressName)); err == nil {
				var p progress
				if json.Unmarshal(data, &p) == nil && p.Program == "fieldcut" {
					continue
				}
			}
		}
		return media.Newf(media.KindConflict, "export.open", "", -1, "refusing to overwrite non-fieldcut content %q", e.Name())
	}
	frames, err := os.ReadDir(filepath.Join(dir, "frames"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return media.New(media.KindConflict, "export.open", "", -1, err)
	}
	for _, e := range frames {
		if _, ok := parseFrameName(e.Name()); !ok || e.IsDir() {
			return media.Newf(media.KindConflict, "export.open", "", -1, "refusing to overwrite non-fieldcut frame %q", e.Name())
		}
	}
	return nil
}

// clearOwned removes only files this program created: numbered PNG frames,
// their temp files and the ownership manifest.
func clearOwned(dir string) error {
	frames, err := os.ReadDir(filepath.Join(dir, "frames"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return media.New(media.KindConflict, "export.invalidate", "", -1, err)
	}
	for _, e := range frames {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if _, ok := parseFrameName(name); !ok && !strings.HasSuffix(name, ".png.tmp") && !strings.HasSuffix(name, ".png") {
			return media.Newf(media.KindConflict, "export.invalidate", "", -1, "refusing to delete foreign frame %q", name)
		}
		if err := os.Remove(filepath.Join(dir, "frames", name)); err != nil {
			return media.New(media.KindConflict, "export.invalidate", "", -1, err)
		}
	}
	if err := os.Remove(filepath.Join(dir, ProgressName)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return media.New(media.KindConflict, "export.invalidate", "", -1, err)
	}
	if err := os.Remove(filepath.Join(dir, ManifestName)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return media.New(media.KindConflict, "export.invalidate", "", -1, err)
	}
	return nil
}

// adopt re-hashes frames left by an interrupted run and records them, so the
// resumed pass skips regenerating them without trusting unverified bytes.
func (e *Exporter) adopt() error {
	frames, err := os.ReadDir(filepath.Join(e.dir, "frames"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return media.New(media.KindConflict, "export.adopt", "", -1, err)
	}
	for _, ent := range frames {
		if ent.IsDir() {
			continue
		}
		seq, ok := parseFrameName(ent.Name())
		if !ok {
			continue
		}
		path := filepath.Join(e.dir, "frames", ent.Name())
		sum, err := hashFile(path)
		if err != nil {
			return media.New(media.KindConflict, "export.adopt", "", seq, err)
		}
		e.partial[seq] = sum
	}
	return nil
}

// parseFrameName recognises the canonical 00000.png frame name.
func parseFrameName(name string) (int64, bool) {
	if len(name) != 9 || !strings.HasSuffix(name, ".png") || strings.HasSuffix(name, ".png.tmp") {
		return 0, false
	}
	digits := name[:5]
	var seq int64
	for _, c := range digits {
		if c < '0' || c > '9' {
			return 0, false
		}
		seq = seq*10 + int64(c-'0')
	}
	return seq, true
}
