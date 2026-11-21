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
	VideoDir       string
	AudioDir       string
	RTPDir         string
	RTPFile        string
	MissingIdx     int
	CorruptIdx     int
	JumpIdx        int
	OffsetIdx      int
	KeyframeGapIdx int
	WarnSegs       int
}

// Video and audio track constants for the generated fixtures.
const (
	VideoTimescale = 90000
	AudioTimescale = 48000
	SegmentCount   = 12
	SegmentSeconds = 2
	VideoRate      = 30
	AudioRate      = 48000
	AudioFrame     = 1024
	// AudioBaseOffset is a deliberate constant 10 ms A/V clock offset applied
	// from OffsetIdx onward.
	AudioBaseOffset = AudioTimescale / 100
	// AudioJumpOffset is an extra 20 ms on JumpIdx only, yielding a 30 ms
	// maximum sync error that stays within the 40 ms demo budget.
	AudioJumpOffset = AudioTimescale / 50
)

func videoSeg(idx int) string { return fmt.Sprintf("media/video/seg-%02d.m4s", idx) }
func audioSeg(idx int) string { return fmt.Sprintf("media/audio/seg-%02d.m4s", idx) }

// NewLayout returns the canonical demo layout under work.
func NewLayout(work string) Layout {
	return Layout{
		Work:           work,
		HLS:            filepath.Join(work, "hls", "media.m3u8"),
		DASH:           filepath.Join(work, "dash", "manifest.mpd"),
		VideoInit:      filepath.Join(work, "media", "init-v.mp4"),
		AudioInit:      filepath.Join(work, "media", "init-a.mp4"),
		VideoDir:       filepath.Join(work, "media", "video"),
		AudioDir:       filepath.Join(work, "media", "audio"),
		RTPDir:         filepath.Join(work, "rtp"),
		RTPFile:        filepath.Join(work, "rtp", "capture.rtp"),
		MissingIdx:     6,
		CorruptIdx:     2,
		JumpIdx:        7,
		OffsetIdx:      4,
		KeyframeGapIdx: 8,
		WarnSegs:       4,
	}
}

// Generate writes all demo inputs and returns the layout. It is deterministic:
// running it twice produces byte-identical files.
func Generate(work string) (Layout, error) {
	l := NewLayout(work)
	for _, dir := range []string{l.VideoDir, l.AudioDir, l.HLS, l.DASH, l.RTPDir} {
		if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
			return l, err
		}
	}
	videoSamples := SegmentSeconds * VideoRate
	audioSamples := SegmentSeconds * AudioRate / AudioFrame
	if err := writeFile(l.VideoInit, initFile(VideoTimescale, AudioTimescale, SegmentCount*SegmentSeconds*1000)); err != nil {
		return l, err
	}
	if err := writeFile(l.AudioInit, initFile(VideoTimescale, AudioTimescale, SegmentCount*SegmentSeconds*1000)); err != nil {
		return l, err
	}
	for i := 0; i < SegmentCount; i++ {
		if i == l.MissingIdx {
			continue // referenced by the manifest, deliberately absent on disk
		}
		video := SegmentSpec{
			TrackID: 1, BaseTime: uint64(i * SegmentSeconds * VideoTimescale),
			SampleDur: VideoTimescale / VideoRate, SampleCount: videoSamples,
			SampleSize: 700, Keyframe: i != l.KeyframeGapIdx,
		}
		switch i {
		case l.CorruptIdx:
			video.CorruptBox = true // impossible trun size -> undecodable range
		case l.CorruptIdx + 1:
			video.Truncate = 37 // truncated trailing mdat
		}
		if err := writeFile(filepath.Join(work, videoSeg(i)), segmentFile(video, uint32(i+1))); err != nil {
			return l, err
		}
	}
	for i := 0; i < SegmentCount; i++ {
		offset := uint64(0)
		if i >= l.OffsetIdx {
			offset = AudioBaseOffset
		}
		if i == l.JumpIdx {
			offset += AudioJumpOffset
		}
		audio := SegmentSpec{
			TrackID: 2, BaseTime: uint64(i*SegmentSeconds*AudioTimescale) + offset,
			SampleDur: AudioFrame, SampleCount: audioSamples,
			SampleSize: 180, Keyframe: true,
		}
		if err := writeFile(filepath.Join(work, audioSeg(i)), segmentFile(audio, uint32(i+1))); err != nil {
			return l, err
		}
	}
	// The missing segment is simply never written; its timeline slot remains.
	if err := writeFile(l.HLS, hlsPlaylist(l)); err != nil {
		return l, err
	}
	if err := writeFile(l.DASH, dashManifest(l)); err != nil {
		return l, err
	}
	return l, nil
}
