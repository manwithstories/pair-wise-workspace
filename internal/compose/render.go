// Package compose renders the canonical video frame sequence and PCM mix from
// resolved edits, working in bounded frame blocks.
package compose

import (
	"image"
	"math"
	"sort"

	"pairflow/internal/audio"
	"pairflow/internal/media"
	"pairflow/internal/timeline"
	"pairflow/internal/video"
)

// Frame is one rendered output frame before encoding.
type Frame struct {
	Index int
	Image *image.NRGBA
	// Layers names the contributing clip(s), for provenance and continuity
	// checks. A dissolve lists both clips.
	Layers []string
	// Blend is the dissolve weight (0 = first layer only).
	Blend float64
}

// VideoTrack is a resolved track prepared for compositing.
type VideoTrack struct {
	ID       string
	Segments []timeline.Segment
	// Priority orders tracks when several are active: lower wins the base
	// layer. cam-a intentionally sits above cam-b.
	Priority int
}

// PreparedVideo holds normalized segment copies for fast lookup.
type preparedSeg struct {
	seg      timeline.Segment
	space    video.Space
	startFrm int64
	endFrm   int64
}

// PrepareVideo validates every segment's video description once, before any
// frame is rendered, so an unknown space fails structurally rather than
// halfway through the sequence.
func PrepareVideo(tracks []timeline.Resolved, order map[string]int) ([]VideoTrack, *media.Error) {
	out := make([]VideoTrack, 0, len(tracks))
	for _, tr := range tracks {
		if tr.Kind != "video" {
			continue
		}
		vt := VideoTrack{ID: tr.TrackID, Segments: tr.Segments, Priority: order[tr.TrackID]}
		for i, seg := range tr.Segments {
			if seg.Video == nil {
				return nil, media.E(media.ClassInternal, "compose.no_video", "video segment %s lacks a video spec", seg.ClipID)
			}
			if err := video.ValidatePattern(seg.Video.Pattern, seg.Video.Tint,
				tr.TrackID+"/"+seg.ClipID, "video.pattern"); err != nil {
				return nil, err
			}
			if _, err := video.ParseSpace(seg.Video.Space, tr.TrackID+"/"+seg.ClipID, "video.space"); err != nil {
				return nil, err
			}
			_, _ = i, seg
		}
		out = append(out, vt)
	}
	if len(out) == 0 {
		return nil, media.Input("compose.no_video_tracks", "timeline has no video tracks to render")
	}
	return out, nil
}

// RenderFrame renders frame index idx across all video tracks. Tracks are
// ordered by ascending priority: the lowest-priority active track supplies the
// base image and each higher-priority active track is composited over it. A
// track's dissolve blends the outgoing and incoming clips of that same track,
// keeping the timeline continuous across frame boundaries.
func RenderFrame(tracks []VideoTrack, idx int) (*Frame, *media.Error) {
	if len(tracks) == 0 {
		return nil, media.E(media.ClassInternal, "compose.no_tracks", "no video tracks prepared")
	}
	ordered := append([]VideoTrack(nil), tracks...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Priority < ordered[j].Priority })
	var canvas *image.NRGBA
	layers := []string{}
	var blend float64
	for _, vt := range ordered {
		img, names, b, err := renderTrack(vt, idx)
		if err != nil {
			return nil, err
		}
		if img == nil {
			continue
		}
		layers = append(layers, names...)
		blend = b
		if canvas == nil {
			canvas = img
		} else {
			// Higher-priority tracks are applied as a full-frame overlay at
			// half opacity so the base track remains identifiable; the image
			// size is fixed at 320x180 for every pattern.
			canvas = video.Blend(canvas, img, 0.5)
		}
	}
	if canvas == nil {
		return nil, media.E(media.ClassInternal, "compose.empty_frame", "frame %d has no active layer", idx)
	}
	return &Frame{Index: idx, Image: canvas, Layers: layers, Blend: blend}, nil
}

// renderTrack returns the frame image for one track at index idx, the clip
// names contributing to it, and the dissolve weight.
func renderTrack(vt VideoTrack, idx int) (*image.NRGBA, []string, float64, *media.Error) {
	for _, seg := range vt.Segments {
		start, stop, err := segFrameRange(seg)
		if err != nil {
			return nil, nil, 0, err
		}
		if int64(idx) < start || int64(idx) >= stop {
			continue
		}
		// Dissolve tail: blend into the next clip.
		if seg.DissolveOut.Num > 0 {
			_d, derr := seg.DissolveOut.FrameIndex()
			if derr != nil {
				return nil, nil, 0, derr
			}
			frames := int64(_d)
			if frames > 0 && int64(idx) >= stop-frames {
				next := nextSegment(vt, seg.ClipID)
				if next != nil {
					t := float64(int64(idx)-(stop-frames)) / float64(frames)
					a, aerr := renderSegment(seg, idx)
					if aerr != nil {
						return nil, nil, 0, aerr
					}
					b, berr := renderSegment(*next, idx)
					if berr != nil {
						return nil, nil, 0, berr
					}
					return video.Blend(a, b, t), []string{seg.ClipID, next.ClipID}, t, nil
				}
			}
		}
		img, rerr := renderSegment(seg, idx)
		if rerr != nil {
			return nil, nil, 0, rerr
		}
		if seg.DissolveIn.Num > 0 {
			_d, _ := seg.DissolveIn.FrameIndex()
			frames := int64(_d)
			if frames > 0 && int64(idx) < start+frames {
				t := float64(int64(idx)-start) / float64(frames)
				return img, []string{seg.ClipID}, t, nil
			}
		}
		return img, []string{seg.ClipID}, 0, nil
	}
	return nil, nil, 0, nil
}

func segFrameRange(seg timeline.Segment) (int64, int64, *media.Error) {
	s, err := seg.Start.FrameIndex()
	if err != nil {
		return 0, 0, err
	}
	e, err := seg.End.FrameIndex()
	if err != nil {
		return 0, 0, err
	}
	return s, e, nil
}

func nextSegment(vt VideoTrack, clipID string) *timeline.Segment {
	for i := range vt.Segments {
		if vt.Segments[i].ClipID == clipID && i+1 < len(vt.Segments) {
			return &vt.Segments[i+1]
		}
	}
	return nil
}

func renderSegment(seg timeline.Segment, idx int) (*image.NRGBA, *media.Error) {
	spec := seg.Video
	space, err := video.ParseSpace(spec.Space, seg.TrackID+"/"+seg.ClipID, "video.space")
	if err != nil {
		return nil, err
	}
	// Frame counter is relative to the segment start so each clip animates
	// from its own first frame; the compositor never depends on wall time.
	start, _ := seg.Start.FrameIndex()
	local := int(int64(idx) - start)
	if local < 0 {
		local = 0
	}
	raw := video.Pattern(spec.Pattern, spec.Tint, space, local)
	norm, nerr := video.Normalize(raw, spec.Space, seg.TrackID+"/"+seg.ClipID)
	if nerr != nil {
		return nil, nerr
	}
	return norm, nil
}

// AudioTrack is a resolved audio track prepared for mixing.
type AudioTrack struct {
	ID       string
	Gain     float64
	Clips    []audio.ClipMix
	Segments []timeline.Segment
}

// PrepareAudio validates audio specs and converts segment spans to canonical
// frames. A cut at e.g. 44.1 kHz maps through the shared rational base; the
// rounded boundary is within one canonical sample of the exact cut.
func PrepareAudio(tracks []timeline.Resolved) ([]AudioTrack, map[string]timeline.Segment, *media.Error) {
	out := make([]AudioTrack, 0, len(tracks))
	for _, tr := range tracks {
		if tr.Kind != "audio" {
			continue
		}
		at := AudioTrack{ID: tr.TrackID, Gain: 0.6}
		if tr.TrackID == "remote" {
			at.Gain = 0.4
		}
		for _, seg := range tr.Segments {
			if seg.Audio == nil {
				return nil, nil, media.E(media.ClassInternal, "compose.no_audio", "audio segment %s lacks an audio spec", seg.ClipID)
			}
			src, err := audio.NewSource(seg.Audio.Rate, seg.Audio.Channels, seg.Audio.Label, tr.TrackID+"/"+seg.ClipID)
			if err != nil {
				return nil, nil, err
			}
			startS, err := seg.Start.SampleIndex()
			if err != nil {
				return nil, nil, err
			}
			endS, err := seg.End.SampleIndex()
			if err != nil {
				return nil, nil, err
			}
			var dIn, dOut int64
			if seg.DissolveIn.Num > 0 {
				dIn, _ = seg.DissolveIn.SampleIndex()
			}
			if seg.DissolveOut.Num > 0 {
				dOut, _ = seg.DissolveOut.SampleIndex()
			}
			at.Clips = append(at.Clips, audio.ClipMix{
				Span:        audio.Span{Start: startS, End: endS},
				Source:      src,
				Label:       tr.TrackID + "/" + seg.ClipID,
				DissolveIn:  dIn,
				DissolveOut: dOut,
			})
		}
		out = append(out, at)
	}
	return out, nil, nil
}

// RenderPCM renders the audio mix in bounded blocks and returns the whole
// interleaved buffer. totalFrames is the timeline's canonical sample count.
func RenderPCM(tracks []AudioTrack, totalFrames int64, blockFrames int) ([]int16, *media.Error) {
	if blockFrames <= 0 {
		return nil, media.Input("compose.block_size", "block size must be positive, got %d", blockFrames)
	}
	out := make([]int16, totalFrames*int64(media.AudioChannels))
	for start := int64(0); start < totalFrames; start += int64(blockFrames) {
		n := int64(blockFrames)
		if start+n > totalFrames {
			n = totalFrames - start
		}
		block := make([]int16, n*int64(media.AudioChannels))
		for i := range tracks {
			if err := audio.MixClips(block, start, tracks[i].Clips, tracks[i].Gain); err != nil {
				return nil, err
			}
		}
		copy(out[start*int64(media.AudioChannels):], block)
	}
	return out, nil
}

// FrameCount is the shared frame count of a timeline.
func FrameCount(dur media.MediaTime) (int, *media.Error) {
	f, err := dur.FrameIndex()
	if err != nil {
		return 0, err
	}
	return int(f), nil
}

// SampleCount is the shared sample count of a timeline.
func SampleCount(dur media.MediaTime) (int64, *media.Error) {
	return dur.SampleIndex()
}

// Clamp01 keeps a dissolve weight in range.
func Clamp01(v float64) float64 { return math.Max(0, math.Min(1, v)) }
