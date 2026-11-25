package selftest

import (
	"os"
	"path/filepath"
	"regexp"

	"medpipe/internal/resume"
)

// readOutDir lists the committed segment files under <dir>/out.
func readOutDir(dir string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(dir, "out"))
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names, nil
}

// hashSegZero returns the SHA-256 of out/seg-000.mp4.
func hashSegZero(dir string) (string, error) {
	return resume.HashFile(filepath.Join(dir, "out", "seg-000.mp4"))
}

// hex64 matches the required 64-character lowercase hex digest.
var hex64 = regexp.MustCompile(`^[0-9a-f]{64}$`)

// validSha256 reports whether digest is a well-formed SHA-256 hex string.
func validSha256(digest string) bool { return hex64.MatchString(digest) }
