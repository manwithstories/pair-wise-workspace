package audit_test

import (
	"os"
	"path/filepath"
	"testing"

	"mediqc/internal/audit"
	"mediqc/internal/pipeline"
)

func TestVerifyDirectoryIndependentlyMatchesManifest(t *testing.T) {
	dir := t.TempDir()
	if _, err := pipeline.Run(pipeline.Options{Seed: 412, Suite: "all", Out: dir}); err != nil {
		t.Fatal(err)
	}
	v, err := audit.VerifyDirectory(dir)
	if err != nil {
		t.Fatal(err)
	}
	if v.ManifestHash == "" || v.Manifest.SampleHashMatch != "100%" {
		t.Fatalf("verification result=%+v", v)
	}

	manifestBytes, ioErr := os.ReadFile(filepath.Join(dir, "suite-manifest.json"))
	if ioErr != nil {
		t.Fatal(ioErr)
	}
	if got := audit.HashBytes(manifestBytes); got != v.ManifestHash {
		t.Fatal("verify did not print the exact manifest byte hash")
	}
}

func TestVerifyDetectsTamperedOutput(t *testing.T) {
	dir := t.TempDir()
	m, perr := pipeline.Run(pipeline.Options{Seed: 7, Suite: "all", Out: dir})
	if perr != nil {
		t.Fatal(perr)
	}
	var tsPath string
	for _, cm := range m.CasesManifests {
		if cm.OutputTS != nil {
			tsPath = filepath.Join(dir, cm.OutputTS.Path)
			break
		}
	}
	if tsPath == "" {
		t.Fatal("no accepted TS output")
	}
	f, err := os.OpenFile(tsPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte{0xff}); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if _, err := audit.VerifyDirectory(dir); err == nil {
		t.Fatal("tampered output must fail independent verification")
	}
}
