package main

import "mediaslice/stream"

// ReadManifestForTest exposes manifest loading to integration tests.
func ReadManifestForTest(workDir string) (*stream.Manifest, error) {
	return stream.ReadManifest(workDir)
}
