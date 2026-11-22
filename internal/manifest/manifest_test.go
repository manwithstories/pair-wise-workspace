package manifest

import (
	"os"
	"path/filepath"
	"testing"

	"mediabridge/internal/fault"
)

func writeManifest(t *testing.T, dir, body string) string {
	t.Helper()
	path := filepath.Join(dir, "manifest.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadResolvesAndConfines(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "clip.mp4"), []byte("x"), 0o644)
	path := writeManifest(t, dir, `{
	  "version": 1,
	  "root_dir": ".",
	  "assets": [{
	    "id": "A1",
	    "primary": {"format": "mp4", "path": "clip.mp4"},
	    "mapping": {"auxiliary": [], "drop_unmapped": true}
	  }]
	}`)
	l, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	assets := l.Assets()
	if len(assets) != 1 || assets[0].ID != "A1" {
		t.Fatalf("assets = %+v", assets)
	}
	if filepath.Base(assets[0].PrimaryPath) != "clip.mp4" {
		t.Fatalf("primary = %s", assets[0].PrimaryPath)
	}
}

func TestLoadRejectsEscape(t *testing.T) {
	dir := t.TempDir()
	path := writeManifest(t, dir, `{
	  "version": 1,
	  "root_dir": ".",
	  "assets": [{"id": "A1", "primary": {"path": "../escape.mp4"}, "mapping": {}}]
	}`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("want escape rejection")
	}
	if fault.CodeOf(err) != fault.CodePath {
		t.Fatalf("code = %v (%v)", fault.CodeOf(err), err)
	}
}

func TestLoadRejectsDuplicateAuxNames(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "clip.mp4"), []byte("x"), 0o644)
	path := writeManifest(t, dir, `{
	  "version": 1,
	  "assets": [{
	    "id": "A1",
	    "primary": {"path": "clip.mp4"},
	    "mapping": {
	      "auxiliary": [{"name": "tc"}, {"name": "tc"}]
	    }
	  }]
	}`)
	_, err := Load(path)
	if fault.CodeOf(err) != fault.CodeDuplicateMapping {
		t.Fatalf("code = %v (%v)", fault.CodeOf(err), err)
	}
}

func TestLoadRejectsUnknownAuxSource(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "clip.mp4"), []byte("x"), 0o644)
	path := writeManifest(t, dir, `{
	  "version": 1,
	  "assets": [{
	    "id": "A1",
	    "primary": {"path": "clip.mp4"},
	    "mapping": {"auxiliary": [{"name": "tc", "source": "nope.rtpd"}]}
	  }]
	}`)
	_, err := Load(path)
	if fault.CodeOf(err) != fault.CodeMissingMapping {
		t.Fatalf("code = %v (%v)", fault.CodeOf(err), err)
	}
}

func TestLoadRejectsAbsoluteOutsideRoot(t *testing.T) {
	dir := t.TempDir()
	path := writeManifest(t, dir, `{
	  "version": 1,
	  "assets": [{"id": "A1", "primary": {"path": "/etc/passwd"}, "mapping": {}}]
	}`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("want rejection of absolute path outside root")
	}
}

func TestLoadBadJSON(t *testing.T) {
	dir := t.TempDir()
	path := writeManifest(t, dir, `{not json`)
	_, err := Load(path)
	if fault.CodeOf(err) != fault.CodeCorruptContainer {
		t.Fatalf("code = %v", fault.CodeOf(err))
	}
}
