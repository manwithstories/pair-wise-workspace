package pipeline

import (
	"relayctl/internal/checksum"
)

// buildDigest rebuilds the checksum manifest over the output directory and
// returns its stable overall digest.
func buildDigest(outDir string) (string, error) {
	m, err := checksum.Build(outDir, outManifest)
	if err != nil {
		return "", err
	}
	if _, err := checksum.Write(outDir, outManifest, m); err != nil {
		return "", err
	}
	return m.ManifestDigest, nil
}
