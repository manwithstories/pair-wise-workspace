package main

import "mediad/internal/index"

// indexDigest recomputes a source file's SHA-256 for independent
// verification against the manifest's recorded input digest.
func indexDigest(path string) (string, error) {
	return index.HashFile(path)
}
