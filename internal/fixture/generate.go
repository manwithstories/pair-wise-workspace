package fixture

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"bq/internal/ffmpeg"
	"bq/internal/manifest"
)

// mediaFrameCount bounds how many frames the real MP4 fixtures carry. The
// manifest timeline still spans the full duration; these frames anchor the
// renderer's sparse-keyframe seek verification.
const mediaFrameCount = 36

// Generate writes the manifest and all media/sample files into outDir.
// Everything is created below outDir so nothing escapes the chosen output.
func Generate(ctx context.Context, m *manifest.Manifest, outDir string) error {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return fmt.Errorf("create output dir: %w", err)
	}
	mediaDir := filepath.Join(outDir, "media")
	if err := os.MkdirAll(mediaDir, 0o755); err != nil {
		return err
	}
	concatDir := filepath.Join(outDir, "work")
	if err := os.MkdirAll(concatDir, 0o755); err != nil {
		return err
	}

	for i := range m.Streams {
		s := &m.Streams[i]
		outPath := filepath.Join(mediaDir, filepath.Base(s.File))
		if s.Kind == manifest.KindVideo {
			if err := genVideo(ctx, s, outPath, concatDir); err != nil {
				return err
			}
		} else {
			if err := ffmpeg.GenerateAudioMP4(ctx, outPath, int(clampDuration(m.DurationS)), int(s.TimebaseNum)); err != nil {
				return err
			}
		}
	}
	return writeManifest(m, filepath.Join(outDir, "manifest.json"))
}

func genVideo(ctx context.Context, s *manifest.Stream, outPath, workDir string) error {
	// VFR durations oscillate between 0.5s and 1.5s, deterministically.
	durations := make([]float64, mediaFrameCount)
	for i := range durations {
		if i%2 == 0 {
			durations[i] = 0.5
		} else {
			durations[i] = 1.5
		}
	}
	concat := filepath.Join(workDir, fmt.Sprintf("concat-%d.txt", s.ID))
	spec := ffmpeg.VideoSpec{
		OutPath: outPath, Width: s.Width, Height: s.Height,
		FrameCount: mediaFrameCount, FrameDurations: durations,
		KeyframeEvery: s.KeyframeEvery,
		StartPTS:      float64(s.StartPTS) / float64(s.TimebaseNum),
	}
	return ffmpeg.GenerateVFRMP4(ctx, spec, concat)
}

func clampDuration(d int64) int64 {
	if d > 30 {
		return 30 // keep audio fixture small; timeline length lives in manifest
	}
	return d
}

func writeManifest(m *manifest.Manifest, path string) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}
	return nil
}

// WriteManifest exposes manifest serialization for the damage-case builder.
func WriteManifest(m *manifest.Manifest, path string) error {
	return writeManifest(m, path)
}
