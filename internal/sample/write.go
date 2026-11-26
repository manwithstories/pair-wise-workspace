package sample

import (
	"fmt"
	"os"
	"path/filepath"
)

// WriteFixtures writes deterministic generated binary fixtures into dir.
func WriteFixtures(dir string) ([]string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	mp4 := BuildMP4Fixtures()
	mxf := BuildMXFFixtures()
	files := []struct {
		name string
		data []byte
	}{
		{"normal.mp4", mp4.Normal},
		{"crop_out_of_bounds.mp4", mp4.BadCrop},
		{"rotation_illegal.mp4", mp4.BadRotation},
		{"normal.mxf", mxf.Normal},
		{"truncated_klv.mxf", mxf.TruncatedKLV},
	}
	written := make([]string, 0, len(files))
	for _, f := range files {
		path := filepath.Join(dir, f.name)
		if err := os.WriteFile(path, f.data, 0o644); err != nil {
			return written, fmt.Errorf("write %s: %w", path, err)
		}
		written = append(written, path)
	}
	return written, nil
}
