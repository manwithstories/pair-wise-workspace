// Package fixtures builds deterministic, self-contained MP4 samples for the
// demo and tests. Nothing is loaded from disk; every byte, including the
// corruption variants, is synthesised from the seed.
package fixtures

import (
	"encoding/binary"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"

	mp4 "github.com/abema/go-mp4"
)

// Variant selects which sanitised sample to build.
type Variant string

const (
	// VariantNormal is a healthy multi-track asset.
	VariantNormal Variant = "normal"
	// VariantTruncated cuts the media data tail, leaving trailing samples that
	// point past EOF.
	VariantTruncated Variant = "truncated"
	// VariantOutOfRange points one sample offset far beyond EOF.
	VariantOutOfRange Variant = "out-of-range"
	// VariantMissingPages zeroes a contiguous media range, as if a page were
	// missing from the file.
	VariantMissingPages Variant = "missing-pages"
)

// AllVariants lists the four generated sample classes in demo order.
func AllVariants() []Variant {
	return []Variant{VariantNormal, VariantTruncated, VariantOutOfRange, VariantMissingPages}
}

// Spec controls the generated media.
type Spec struct {
	Seed int64
	// VideoSamples and AudioSamples are sample counts.
	VideoSamples int
	AudioSamples int
	// KeyframeEvery controls keyframe sparsity. Values larger than the sample
	// count produce a single leading keyframe.
	KeyframeEvery int
	// OriginMS is the non-zero presentation origin applied to both tracks.
	OriginMS int64
	// Layout is the declared audio layout; "unknown" exercises the fallback.
	Layout string
	// TruncateTailBytes is how many bytes to cut from the video tail for the
	// truncated variant.
	TruncateTailBytes int
	// KeepExisting reuses media files that already exist on disk, so an
	// operator can mutate a fixture and observe cache invalidation.
	KeepExisting bool
	// DurationScale multiplies the default sample counts, letting tests
	// generate long material without changing the per-sample shape.
	DurationScale int
}

// DefaultSpec returns the standard four-class demo spec for a seed.
func DefaultSpec(seed int64) Spec {
	return Spec{
		Seed:              seed,
		VideoSamples:      72,
		AudioSamples:      135,
		KeyframeEvery:     12,
		OriginMS:          1234,
		Layout:            "stereo",
		TruncateTailBytes: 4096,
	}
}

// Asset is one built sample: media files plus the manifest that references
// them.
type Asset struct {
	Variant    Variant
	VideoPath  string
	AudioPath  string
	Manifest   string
	VideoBytes int64
	AudioBytes int64
}

// Build writes one variant (both tracks plus a manifest) into dir.
func Build(dir string, spec Spec, v Variant) (*Asset, error) {
	if spec.VideoSamples <= 0 || spec.AudioSamples <= 0 {
		return nil, fmt.Errorf("fixtures: sample counts must be positive")
	}
	if spec.KeyframeEvery < 0 {
		return nil, fmt.Errorf("fixtures: negative keyframe interval")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	base := string(v)
	vpath := filepath.Join(dir, base+"-video.mp4")
	apath := filepath.Join(dir, base+"-audio.mp4")

	videoRng := rand.New(rand.NewSource(spec.Seed))
	audioRng := rand.New(rand.NewSource(spec.Seed ^ 0x5f3759df))

	// The first video edit is an empty edit that shifts presentation start by
	// 120 ms, exercising the edit-mapping path.
	videoEdits := []mp4.ElstEntry{
		{SegmentDurationV0: 120, MediaTimeV0: -1, MediaRateInteger: 1},
		{SegmentDurationV0: uint32(spec.VideoSamples * 40), MediaTimeV0: 0, MediaRateInteger: 1},
	}
	video, err := buildTrack(videoRng, trackPlan{
		kind: "video", trackID: 1, timescale: videoTimescale,
		count: spec.VideoSamples, delta: videoFrameTick,
		keyEvery: spec.KeyframeEvery, emptyEdits: videoEdits,
	})
	if err != nil {
		return nil, fmt.Errorf("fixtures: build video: %w", err)
	}
	// Apply the same leading empty edit to audio so both tracks share one
	// presentation shift and remain synchronised.
	audioEdits := []mp4.ElstEntry{
		{SegmentDurationV0: 120, MediaTimeV0: -1, MediaRateInteger: 1},
		{SegmentDurationV0: uint32(spec.AudioSamples * audioFrame * 1000 / audioTimescale), MediaTimeV0: 0, MediaRateInteger: 1},
	}
	audio, err := buildTrack(audioRng, trackPlan{
		kind: "audio", trackID: 2, timescale: audioTimescale,
		count: spec.AudioSamples, delta: audioFrame, keyEvery: 0,
		emptyEdits: audioEdits,
		// 1024 stereo frames of 16-bit PCM per sample.
		payloadBytes: audioFrame * 2 * 2,
		pcm:          true,
	})
	if err != nil {
		return nil, fmt.Errorf("fixtures: build audio: %w", err)
	}
	video, audio, err = corrupt(v, video, audio, spec.TruncateTailBytes)
	if err != nil {
		return nil, err
	}
	if err := writeUnlessKept(vpath, video, spec.KeepExisting); err != nil {
		return nil, err
	}
	if err := writeUnlessKept(apath, audio, spec.KeepExisting); err != nil {
		return nil, err
	}
	man := manifestJSON(base, spec)
	mpath := filepath.Join(dir, base+"-manifest.json")
	if err := os.WriteFile(mpath, []byte(man), 0o644); err != nil {
		return nil, err
	}
	a := &Asset{Variant: v, VideoPath: vpath, AudioPath: apath, Manifest: mpath}
	a.VideoBytes = int64(len(video))
	a.AudioBytes = int64(len(audio))
	return a, nil
}

func manifestJSON(base string, spec Spec) string {
	return fmt.Sprintf(`{
  "version": 1,
  "tracks": [
    {
      "id": 1,
      "kind": "video",
      "source": "%s-video.mp4",
      "required": true,
      "origin_ms": %d,
      "edits": [{"segment_duration_ms": %d, "media_time_ms": 0}]
    },
    {
      "id": 2,
      "kind": "audio",
      "source": "%s-audio.mp4",
      "required": true,
      "origin_ms": %d,
      "audio": {"sample_rate": 48000, "channel_count": 2, "layout": %q}
    }
  ]
}
`, base, spec.OriginMS, spec.VideoSamples*40, base, spec.OriginMS, spec.Layout)
}

// corrupt applies the variant mutation and returns replacement byte slices.
func corrupt(v Variant, video, audio []byte, truncateBytes int) ([]byte, []byte, error) {
	switch v {
	case VariantNormal:
		return video, audio, nil
	case VariantTruncated:
		if truncateBytes <= 0 {
			truncateBytes = 4096
		}
		// Remove bytes from the end so the last samples point past EOF.
		if len(video) <= truncateBytes+1024 {
			return nil, nil, fmt.Errorf("fixtures: video too small to truncate by %d bytes", truncateBytes)
		}
		video = append([]byte(nil), video[:len(video)-truncateBytes]...)
		return video, audio, nil
	case VariantOutOfRange:
		video = append([]byte(nil), video...)
		if err := patchChunkOffset(video, 5, 1<<30); err != nil {
			return nil, nil, fmt.Errorf("fixtures: out-of-range: %w", err)
		}
		return video, audio, nil
	case VariantMissingPages:
		video = append([]byte(nil), video...)
		start := len(video) * 2 / 3
		end := start + 1024
		if end > len(video) {
			end = len(video)
		}
		for i := start; i < end; i++ {
			video[i] = 0
		}
		return video, audio, nil
	default:
		return nil, nil, fmt.Errorf("fixtures: unknown variant %q", v)
	}
}

// writeUnlessKept writes data unless keep is set and path already exists.
func writeUnlessKept(path string, data []byte, keep bool) error {
	if keep {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
	}
	return os.WriteFile(path, data, 0o644)
}

// patchChunkOffset rewrites stco entry n. The writer emits one chunk per
// sample, so entry n corresponds to sample n.
func patchChunkOffset(b []byte, n int, off uint32) error {
	idx := findBox(b, "stco", 0)
	if idx < 0 {
		return fmt.Errorf("stco not found")
	}
	entryCount := int(binary.BigEndian.Uint32(b[idx+12 : idx+16]))
	if n < 0 || n >= entryCount {
		return fmt.Errorf("stco has %d entries, want index %d", entryCount, n)
	}
	pos := idx + 16 + n*4
	binary.BigEndian.PutUint32(b[pos:pos+4], off)
	return nil
}

// findBox locates the first payload occurrence of a 4CC starting at from.
func findBox(b []byte, typ string, from int) int {
	for i := from; i+8 <= len(b); i++ {
		if string(b[i+4:i+8]) == typ {
			return i
		}
	}
	return -1
}
