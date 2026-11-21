package main

import (
	"os"
	"path/filepath"

	"mediacore/internal/manifest"
)

// faultRecorder keeps faults that were diagnosed before a manifest could be
// assembled (parse-stage faults, for example) so the manifest still records
// the exact location rather than losing it with the process exit.
type faultRecorder struct {
	path   string
	faults []manifest.Fault
}

var recorder = &faultRecorder{}

// recordFault stores a fault observed during parsing. It returns the original
// error so callers can keep their control flow.
func recordFault(f *demoFlags, err error) error {
	return recordFaultAt(f, "parse", err)
}

// recordFaultAt stores a fault with the stage that observed it.
func recordFaultAt(f *demoFlags, stage string, err error) error {
	if err == nil {
		return nil
	}
	path := f.manifestPath
	if path == "" {
		path = filepath.Join(".", "tmp", "manifest.json")
	}
	recorder.path = path
	recorder.faults = append(recorder.faults, *manifest.FaultFromError(stage, err))
	// Best-effort persistence: if the manifest cannot be written, the exit
	// code and stdout still carry the classification and offset.
	_ = writeFaultManifest(path, f, err)
	return err
}

// writeFaultManifest writes a manifest containing only what is known at fault
// time: the input fixture, the classified fault with its byte offset, and the
// exit-relevant class.
func writeFaultManifest(path string, f *demoFlags, err error) error {
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if mkErr := os.MkdirAll(dir, 0o755); mkErr != nil {
			return mkErr
		}
	}
	m := &manifest.Manifest{
		Inputs: []string{filepath.Join(".", "tmp", fixtureName(f))},
		Faults: recorder.faults,
	}
	return writeManifest(m, path)
}

func fixtureName(f *demoFlags) string {
	if f.format == "mxf" {
		return "fixture.mxf"
	}
	return "fixture.mp4"
}
