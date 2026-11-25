package synth

import (
	"os"
	"path/filepath"

	"archivectl/internal/container"
	"archivectl/internal/transport"
)

// Files lists the deterministic fixtures produced by Generate.
type Files struct {
	Normal       string
	Fragmented   string
	WrapRTP      string
	BurstRTP     string
	TruncatedMP4 string
}

func writeFile(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Generate creates all local fixtures from one fixed seed.
func Generate(outDir string, seed int64) (Files, error) {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return Files{}, err
	}
	normal := normalAsset(seed)
	normalBytes, err := container.BuildMP4(normal)
	if err != nil {
		return Files{}, err
	}
	files := Files{
		Normal:       filepath.Join(outDir, "normal.mp4"),
		Fragmented:   filepath.Join(outDir, "fragmented.mp4"),
		WrapRTP:      filepath.Join(outDir, "wrap.rtp"),
		BurstRTP:     filepath.Join(outDir, "burst.rtp"),
		TruncatedMP4: filepath.Join(outDir, "truncated.mp4"),
	}
	if err := writeFile(files.Normal, normalBytes); err != nil {
		return files, err
	}

	// Concatenate init and complete fragments as a standalone fMP4 fixture.
	frames, metas, err := packetizeAll(seed, 0)
	if err != nil {
		return files, err
	}
	segments, _, err := transport.AssembleCapture(frames, 64)
	if err != nil {
		return files, err
	}
	var fragmented []byte
	for _, seg := range segments {
		fragmented = append(fragmented, seg.Data...)
	}
	if err := writeFile(files.Fragmented, fragmented); err != nil {
		return files, err
	}
	_ = metas

	wrapFrames, err := WrapCapture(seed)
	if err != nil {
		return files, err
	}
	if err := WriteRTPCapture(files.WrapRTP, wrapFrames); err != nil {
		return files, err
	}

	burstFrames, _, err := BurstCapture(seed)
	if err != nil {
		return files, err
	}
	if err := WriteRTPCapture(files.BurstRTP, burstFrames); err != nil {
		return files, err
	}

	// Cut inside the final mdat while retaining its oversized box header.
	cut := len(normalBytes) - 97
	if cut < 64 {
		cut = len(normalBytes) / 2
	}
	if err := writeFile(files.TruncatedMP4, normalBytes[:cut]); err != nil {
		return files, err
	}
	return files, nil
}
