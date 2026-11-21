package fixture

import (
	"fmt"
	"os"
	"path/filepath"
)

// Layout describes where the generated demo inputs live.
type Layout struct {
	Work           string
	HLS            string
	DASH           string
	VideoInit      string
	AudioInit      string
	MediaDir       string
	RTPFile        string
	MissingIdx     int // zero-based segment index deliberately absent
	CorruptIdx     int // zero-based segment with an impossible trun size
	TruncatedIdx   int // zero-based segment with a shortened mdat
	OffsetIdx      int // audio clock offset starts here
	JumpIdx        int // extra audio jump on this segment only
	KeyframeGapIdx int // video has no keyframe at this segment
	WrapIdx        int // video clock resets backwards at this segment (-1 = off)
	OutOfRangeIdx  int // trun declares more samples than its payload holds (-1 = off)
}

// Track and timing constants for the generated fixtures.
const (
	VideoTimescale = 90000
	AudioTimescale = 48000
	SegmentCount   = 12
	SegmentSeconds = 2
	VideoRate      = 30
	// AudioFrame is 1/30 s at 48 kHz, so audio sample boundaries coincide
	// exactly with video frames and alignment measures clock offset only.
	AudioFrame             = AudioTimescale / VideoRate
	SamplesPerVideoSegment = SegmentSeconds * VideoRate
	SamplesPerAudioSegment = SegmentSeconds * AudioTimescale / AudioFrame
	// AudioBaseOffset is a constant 10 ms audio clock offset applied from
	// OffsetIdx onward.
	AudioBaseOffset = AudioTimescale / 100
	// AudioJumpOffset is an extra 20 ms on JumpIdx, yielding a 30 ms peak
	// sync error that stays inside the 40 ms demo budget.
	AudioJumpOffset = AudioTimescale / 50
)

// SegmentURI is the manifest-relative URI for a segment, 1-based as DASH
// $Number$ expansion produces.
func SegmentURI(idx int) string { return fmt.Sprintf("media/seg-%02d.m4s", idx+1) }

// SegmentPath is the absolute path for a segment.
func SegmentPath(work string, idx int) string { return filepath.Join(work, SegmentURI(idx)) }

// NewLayout returns the canonical demo layout under work.
func NewLayout(work string) Layout {
	return Layout{
		Work:           work,
		HLS:            filepath.Join(work, "hls", "media.m3u8"),
		DASH:           filepath.Join(work, "dash", "manifest.mpd"),
		VideoInit:      filepath.Join(work, "media", "init-v.mp4"),
		AudioInit:      filepath.Join(work, "media", "init-a.mp4"),
		MediaDir:       filepath.Join(work, "media"),
		RTPFile:        filepath.Join(work, "rtp", "capture.rtp"),
		MissingIdx:     6,
		CorruptIdx:     2,
		TruncatedIdx:   3,
		OffsetIdx:      4,
		JumpIdx:        7,
		KeyframeGapIdx: 8,
		WrapIdx:        -1,
		OutOfRangeIdx:  -1,
	}
}

// GenerateWrap writes the demo inputs plus a synthetic 32-bit clock wrap: the
// video base time at WrapIdx restarts near zero after a much larger value.
func GenerateWrap(work string) (Layout, error) {
	l := NewLayout(work)
	l.WrapIdx = 5
	return generate(l)
}

// GenerateOutOfRange writes the demo inputs plus a trun that declares far more
// samples than its payload can hold, modelling an out-of-bounds length field.
func GenerateOutOfRange(work string) (Layout, error) {
	l := NewLayout(work)
	l.OutOfRangeIdx = 4
	return generate(l)
}

// Generate writes all demo inputs and returns the layout. Output is
// deterministic: running it twice produces byte-identical media.
func Generate(work string) (Layout, error) {
	return generate(NewLayout(work))
}

func generate(l Layout) (Layout, error) {
	work := l.Work
	for _, dir := range []string{l.MediaDir, filepath.Dir(l.HLS), filepath.Dir(l.DASH), filepath.Dir(l.RTPFile)} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return l, err
		}
	}
	const totalMS = SegmentCount * SegmentSeconds * 1000
	if err := writeFile(l.VideoInit, initFile(VideoTimescale, AudioTimescale, totalMS)); err != nil {
		return l, err
	}
	if err := writeFile(l.AudioInit, initFile(VideoTimescale, AudioTimescale, totalMS)); err != nil {
		return l, err
	}
	for i := 0; i < SegmentCount; i++ {
		if i == l.MissingIdx {
			continue // referenced by both manifests, deliberately absent
		}
		videoBase := uint64(i * SegmentSeconds * VideoTimescale)
		if i == l.WrapIdx {
			// A 32-bit timestamp wrap restarts the clock just after zero,
			// which is far below every preceding base time.
			videoBase = VideoTimescale / 10
		}
		video := SegmentSpec{
			TrackID: 1, BaseTime: videoBase,
			SampleDur: VideoTimescale / VideoRate, SampleCount: SamplesPerVideoSegment,
			SampleSize: 700, Keyframes: i != l.KeyframeGapIdx,
		}
		if i == l.OutOfRangeIdx {
			video.OutOfRange = true
		}
		audioOffset := uint64(0)
		if i >= l.OffsetIdx {
			audioOffset = AudioBaseOffset
		}
		if i == l.JumpIdx {
			audioOffset += AudioJumpOffset
		}
		audio := SegmentSpec{
			TrackID: 2, BaseTime: uint64(i*SegmentSeconds*AudioTimescale) + audioOffset,
			SampleDur: AudioFrame, SampleCount: SamplesPerAudioSegment,
			SampleSize: 180, AllSync: true,
		}
		specs := []SegmentSpec{video, audio}
		switch i {
		case l.CorruptIdx:
			specs[0].CorruptBox = true
		case l.TruncatedIdx:
			specs[len(specs)-1].Truncate = 37
		}
		if err := writeFile(SegmentPath(work, i), segmentFile(specs, uint32(i+1))); err != nil {
			return l, err
		}
	}
	if err := writeFile(l.HLS, hlsPlaylist(l)); err != nil {
		return l, err
	}
	if err := writeFile(l.DASH, dashManifest(l)); err != nil {
		return l, err
	}
	return l, nil
}
