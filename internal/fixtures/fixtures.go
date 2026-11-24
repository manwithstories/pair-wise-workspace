// Package fixtures builds deterministic, self-contained MP4 samples for the
// demo and tests. Nothing is loaded from disk; every byte, including the
// corruption variants, is synthesised from the seed.
package fixtures

import (
	"encoding/binary"
	"fmt"
	"io"
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

	// The first video edit is an empty edit that delays presentation by 120 ms.
	// The real edit starts at the fixture's declared media start (default 0),
	// exercising both an empty edit and a non-zero media time.
	videoEdits := []mp4.ElstEntry{
		{SegmentDurationV0: 120, MediaTimeV0: -1, MediaRateInteger: 1},
		{SegmentDurationV0: uint32(spec.VideoSamples * 40), MediaTimeV0: 0, MediaRateInteger: 1},
	}
	videoSrc, err := buildTrackFile(videoRng, trackPlan{
		kind: "video", trackID: 1, timescale: videoTimescale,
		count: spec.VideoSamples, delta: videoFrameTick,
		keyEvery: spec.KeyframeEvery, emptyEdits: videoEdits,
	})
	if err != nil {
		return nil, fmt.Errorf("fixtures: build video: %w", err)
	}
	defer os.Remove(videoSrc)
	// Apply the same leading empty edit to audio so both tracks share one
	// presentation shift and remain synchronised.
	audioEdits := []mp4.ElstEntry{
		{SegmentDurationV0: 120, MediaTimeV0: -1, MediaRateInteger: 1},
		{SegmentDurationV0: uint32(spec.AudioSamples * audioFrame * 1000 / audioTimescale), MediaTimeV0: 0, MediaRateInteger: 1},
	}
	audioSrc, err := buildTrackFile(audioRng, trackPlan{
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
	defer os.Remove(audioSrc)
	vBytes, err := publishTrack(videoSrc, vpath, v, true, spec)
	if err != nil {
		return nil, err
	}
	aBytes, err := publishTrack(audioSrc, apath, v, false, spec)
	if err != nil {
		return nil, err
	}
	man := manifestJSON(base, spec)
	mpath := filepath.Join(dir, base+"-manifest.json")
	if err := os.WriteFile(mpath, []byte(man), 0o644); err != nil {
		return nil, err
	}
	a := &Asset{Variant: v, VideoPath: vpath, AudioPath: apath, Manifest: mpath}
	a.VideoBytes = vBytes
	a.AudioBytes = aBytes
	return a, nil
}

// publishTrack applies the variant corruption to one track file and copies it
// to dst. Only a bounded scratch buffer is held, so large fixtures do not
// inflate memory.
func publishTrack(src, dst string, v Variant, isVideo bool, spec Spec) (int64, error) {
	if spec.KeepExisting {
		if st, err := os.Stat(dst); err == nil {
			return st.Size(), nil
		}
	}
	if err := copyFile(src, dst); err != nil {
		return 0, err
	}
	if isVideo {
		if err := applyVariantInPlace(dst, v, spec.TruncateTailBytes); err != nil {
			return 0, err
		}
	}
	st, err := os.Stat(dst)
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
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

// findBox locates the first payload occurrence of a 4CC starting at from.
func findBox(b []byte, typ string, from int) int {
	for i := from; i+8 <= len(b); i++ {
		if string(b[i+4:i+8]) == typ {
			return i
		}
	}
	return -1
}

// applyVariantInPlace applies a corruption variant to a media file, touching
// only the bytes it must change so large fixtures are not loaded wholesale.
func applyVariantInPlace(path string, v Variant, truncateBytes int) error {
	switch v {
	case VariantNormal:
		return nil
	case VariantTruncated:
		if truncateBytes <= 0 {
			truncateBytes = 4096
		}
		st, err := os.Stat(path)
		if err != nil {
			return err
		}
		if st.Size() <= int64(truncateBytes)+1024 {
			return fmt.Errorf("fixtures: file too small to truncate by %d bytes", truncateBytes)
		}
		return os.Truncate(path, st.Size()-int64(truncateBytes))
	case VariantOutOfRange:
		off, err := stcoEntryOffset(path, 5)
		if err != nil {
			return err
		}
		f, err := os.OpenFile(path, os.O_WRONLY, 0)
		if err != nil {
			return err
		}
		defer f.Close()
		var buf [4]byte
		binary.BigEndian.PutUint32(buf[:], 1<<30)
		_, err = f.WriteAt(buf[:], off)
		return err
	case VariantMissingPages:
		st, err := os.Stat(path)
		if err != nil {
			return err
		}
		start := st.Size() * 2 / 3
		end := start + 1024
		if end > st.Size() {
			end = st.Size()
		}
		f, err := os.OpenFile(path, os.O_WRONLY, 0)
		if err != nil {
			return err
		}
		defer f.Close()
		zeros := make([]byte, end-start)
		_, err = f.WriteAt(zeros, start)
		return err
	default:
		return fmt.Errorf("fixtures: unknown variant %q", v)
	}
}

// stcoEntryOffset locates the file offset of stco entry n by scanning the moov
// region in bounded chunks.
func stcoEntryOffset(path string, n int) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	// The moov box precedes mdat in generated files; read up to the first mdat.
	head := make([]byte, 2<<20)
	read, _ := io.ReadFull(f, head)
	head = head[:read]
	idx := findBox(head, "stco", 0)
	if idx < 0 {
		return 0, fmt.Errorf("fixtures: stco not found")
	}
	if idx+16 > len(head) {
		return 0, fmt.Errorf("fixtures: stco header truncated")
	}
	count := int(binary.BigEndian.Uint32(head[idx+12 : idx+16]))
	if n < 0 || n >= count {
		return 0, fmt.Errorf("fixtures: stco has %d entries, want %d", count, n)
	}
	return int64(idx + 16 + n*4), nil
}
