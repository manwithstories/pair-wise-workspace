// Package ingest validates source metadata and reads the binary transport
// container produced by the local fixture generator. No network access and
// no path outside the approved input root are ever touched.
package ingest

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"avpipe/internal/media"
)

// Hard safety limits applied to every locally generated or user-supplied
// source. They bound memory and disk usage of a single pipeline run.
const (
	MaxFrames          uint32 = 100_000
	MaxPacketSize             = 8 << 20 // 8 MiB per packet (one raw I420 frame)
	MaxTotalBytes      int64  = 4 << 30 // 4 GiB per source container
	MaxPathLength             = 4096
	SupportedFPS              = 30
	SupportedAudioRate        = 48000
)

// Manifest is the metadata header describing one source asset.
type Manifest struct {
	Frames     uint32 // expected video frame count
	FPS        uint32
	Width      uint16
	Height     uint16
	AudioRate  uint32
	Packets    uint32 // total transport records (video, audio, parity)
	TotalBytes int64  // container file size on disk
	SourcePath string // absolute, cleaned path inside the input root
}

// SafePath resolves name against root and guarantees the result stays inside
// root. System-level symlinks above the deepest existing ancestor are
// resolved to their real path; any symlink inside the managed subtree is
// rejected as an escape attempt. Absolute names are accepted only when they
// are contained in root.
func SafePath(root, name string) (string, error) {
	if len(name) == 0 || len(name) > MaxPathLength {
		return "", media.NewError(media.ClassValidation, "ingest", "invalid path length", 0, false)
	}
	clean := filepath.Clean(name)
	for _, part := range strings.Split(clean, string(filepath.Separator)) {
		if part == ".." {
			return "", media.NewError(media.ClassValidation, "ingest", "path traversal rejected: "+name, 0, false)
		}
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", media.NewError(media.ClassValidation, "ingest", "cannot resolve input root", 0, false)
	}
	var target string
	if filepath.IsAbs(clean) {
		target = clean
	} else {
		target = filepath.Join(absRoot, clean)
	}
	// The real root may itself sit behind a system symlink (/var).
	realRoot, err := filepath.EvalSymlinks(absRoot)
	if err != nil {
		return "", media.NewError(media.ClassValidation, "ingest", "input root not accessible", 0, false)
	}
	// Deepest existing ancestor of the target.
	existing := target
	var tail []string
	for {
		if _, err := os.Lstat(existing); err == nil {
			break
		}
		tail = append([]string{filepath.Base(existing)}, tail...)
		parent := filepath.Dir(existing)
		if parent == existing {
			return "", media.NewError(media.ClassValidation, "ingest", "cannot resolve path: "+name, 0, false)
		}
		existing = parent
	}
	realAncestor, err := filepath.EvalSymlinks(existing)
	if err != nil {
		return "", media.NewError(media.ClassValidation, "ingest", "symlink resolution failed: "+name, 0, false)
	}
	rel, err := filepath.Rel(realRoot, realAncestor)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", media.NewError(media.ClassValidation, "ingest", "path escapes input root: "+name, 0, false)
	}
	resolved := realAncestor
	if len(tail) > 0 {
		resolved = filepath.Join(append([]string{realAncestor}, tail...)...)
	}
	// Reject any symlink within the managed subtree below the ancestor.
	walk := realAncestor
	for _, part := range tail {
		walk = filepath.Join(walk, part)
		if info, err := os.Lstat(walk); err == nil && info.Mode()&os.ModeSymlink != 0 {
			return "", media.NewError(media.ClassValidation, "ingest", "symlink escape rejected: "+walk, 0, false)
		}
	}
	return resolved, nil
}

// Validate checks the manifest against the safety limits and stream
// constraints. root is the approved input directory.
func (m *Manifest) Validate(root string) error {
	if m.Frames == 0 || m.Frames > MaxFrames {
		return media.NewError(media.ClassValidation, "ingest", "frame count out of range", 0, false)
	}
	if m.FPS != SupportedFPS {
		return media.NewError(media.ClassValidation, "ingest", "only 30 fps sources are supported", 0, false)
	}
	if m.AudioRate != SupportedAudioRate {
		return media.NewError(media.ClassValidation, "ingest", "only 48 kHz audio is supported", 0, false)
	}
	if m.Width == 0 || m.Height == 0 || m.Width%2 != 0 || m.Height%2 != 0 {
		return media.NewError(media.ClassValidation, "ingest", "resolution must be positive and even", 0, false)
	}
	if m.Packets == 0 {
		return media.NewError(media.ClassValidation, "ingest", "manifest declares no packets", 0, false)
	}
	if m.TotalBytes <= 0 || m.TotalBytes > MaxTotalBytes {
		return media.NewError(media.ClassValidation, "ingest", "total byte count out of range", 0, false)
	}
	resolved, err := SafePath(root, m.SourcePath)
	if err != nil {
		return err
	}
	m.SourcePath = resolved
	info, err := os.Stat(m.SourcePath)
	if err != nil {
		return media.NewError(media.ClassValidation, "ingest", "source not accessible: "+err.Error(), 0, false)
	}
	if info.IsDir() {
		return media.NewError(media.ClassValidation, "ingest", "source path is a directory", 0, false)
	}
	if info.Size() != m.TotalBytes {
		return media.NewError(media.ClassValidation, "ingest", "declared size does not match file", 0, false)
	}
	return nil
}

// IsCorrupt reports whether err is a checksum/validation failure from ingest.
func IsCorrupt(err error) bool {
	var me *media.Error
	if errors.As(err, &me) {
		return me.Class == media.ClassCorrupt || me.Class == media.ClassValidation
	}
	return false
}
