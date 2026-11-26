package fixtures

import (
	"mediad/internal/manifest"
)

func readManifest(p string) (*manifest.Manifest, error) { return manifest.ParseManifestFile(p) }
func readEdits(p string) (*manifest.EditList, error)    { return manifest.ParseEditsFile(p) }
