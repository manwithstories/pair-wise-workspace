package cut

import (
	"fmt"
	"math"

	"mediapipe/internal/media"
)

// Options configures target-aligned segmentation.
type Options struct {
	TargetSeconds float64
	FrameRate     media.Rat
	VideoStream   int
	AudioStream   int
}

func (o Options) normalize() Options {
	if o.TargetSeconds <= 0 {
		o.TargetSeconds = 10
	}
	if o.FrameRate.N == 0 {
		o.FrameRate = media.NewRat(30000, 1001) // 29.97 broadcast fallback
	}
	return o
}

// Plan describes one output segment and the boundary work needed.
type Plan struct {
	Index             int     `json:"index"`
	Output            string  `json:"output"`
	TargetStartSec    float64 `json:"target_start_sec"`
	TargetEndSec      float64 `json:"target_end_sec"`
	ActualStartSec    float64 `json:"actual_start_sec"`
	ActualEndSec      float64 `json:"actual_end_sec"`
	StartSequence     uint64  `json:"start_sequence"`
	EndSequence       uint64  `json:"end_sequence"`
	KeyFrameAligned   bool    `json:"keyframe_aligned"`
	ReencodeBoundary  bool    `json:"reencode_boundary"`
	ReencodeFromSec   float64 `json:"reencode_from_sec,omitempty"`
	BoundaryFrames    int     `json:"boundary_frames"`
	AudioVideoErrorMS float64 `json:"av_error_ms"`
	WithinOneFrame    bool    `json:"within_one_frame"`
}

type Result struct {
	Segments         []Plan  `json:"segments"`
	AlignmentErrorMS float64 `json:"manifest_error_ms"`
	FrameDurationMS  float64 `json:"frame_duration_ms"`
}

// PlanSegments chooses target-time cuts. A target that does not land on a
// keyframe keeps the exact delivery point but marks the GOP prefix that must be
// decoded/re-encoded, rather than silently moving the cut.
func PlanSegments(packets []media.Packet, opt Options) (Result, error) {
	opt = opt.normalize()
	frameMS := 1000 * media.FrameDuration(opt.FrameRate)
	if frameMS <= 0 {
		return Result{}, media.NewError(media.CodeInvalidTimeBase, "invalid frame rate", nil)
	}
	duration := durationSeconds(packets)
	count := int(math.Ceil(duration / opt.TargetSeconds))
	if count == 0 {
		return Result{FrameDurationMS: frameMS}, nil
	}
	out := Result{FrameDurationMS: frameMS}
	for i := 0; i < count; i++ {
		tStart := float64(i) * opt.TargetSeconds
		tEnd := math.Min(float64(i+1)*opt.TargetSeconds, duration)
		if tEnd-tStart < frameMS/1000 && i > 0 {
			continue
		}
		startPkt := firstAtOrAfter(packets, opt.VideoStream, tStart)
		if startPkt == nil {
			startPkt = firstAtOrAfterAny(packets, tStart)
		}
		endPkt := lastBefore(packets, tEnd)
		if startPkt == nil || endPkt == nil {
			continue
		}
		key := previousKeyframe(packets, opt.VideoStream, tStart)
		p := Plan{
			Index: i, Output: fmt.Sprintf("segment_%03d.mp4", i),
			TargetStartSec: tStart, TargetEndSec: tEnd,
			ActualStartSec: startPkt.PTSSec(), ActualEndSec: endPkt.PTSSec(),
			StartSequence: startPkt.Sequence, EndSequence: endPkt.Sequence,
		}
		if key != nil && math.Abs(key.PTSSec()-tStart) > 1e-6 {
			p.ReencodeBoundary = true
			p.ReencodeFromSec = key.PTSSec()
			p.BoundaryFrames = int(math.Ceil((tStart - key.PTSSec()) / (frameMS / 1000)))
			p.KeyFrameAligned = false
		} else {
			p.KeyFrameAligned = true
			p.BoundaryFrames = 0
		}
		audioPkt := firstStreamAtOrAfter(packets, opt.AudioStream, tStart)
		if audioPkt != nil {
			p.AudioVideoErrorMS = math.Abs(audioPkt.PTSSec()-startPkt.PTSSec()) * 1000
		}
		p.WithinOneFrame = p.AudioVideoErrorMS <= frameMS+1e-9
		if !p.WithinOneFrame {
			return out, media.NewError(media.CodeInvalidInput,
				fmt.Sprintf("segment %d A/V drift %.3fms exceeds one frame %.3fms", i, p.AudioVideoErrorMS, frameMS), nil)
		}
		out.Segments = append(out.Segments, p)
		out.AlignmentErrorMS = math.Max(out.AlignmentErrorMS, p.AudioVideoErrorMS)
	}
	return out, nil
}

func durationSeconds(ps []media.Packet) float64 {
	var max float64
	for _, p := range ps {
		if d := p.PTSSec(); d > max {
			max = d
		}
	}
	return max
}

func firstAtOrAfter(ps []media.Packet, stream int, t float64) *media.Packet {
	for i := range ps {
		if ps[i].StreamIndex == stream && ps[i].PTSSec()+1e-9 >= t {
			return &ps[i]
		}
	}
	return nil
}

func firstStreamAtOrAfter(ps []media.Packet, stream int, t float64) *media.Packet {
	return firstAtOrAfter(ps, stream, t)
}

func firstAtOrAfterAny(ps []media.Packet, t float64) *media.Packet {
	for i := range ps {
		if ps[i].PTSSec()+1e-9 >= t {
			return &ps[i]
		}
	}
	return nil
}

func lastBefore(ps []media.Packet, t float64) *media.Packet {
	var last *media.Packet
	for i := range ps {
		if ps[i].PTSSec() < t {
			last = &ps[i]
		}
	}
	return last
}

func previousKeyframe(ps []media.Packet, videoStream int, t float64) *media.Packet {
	var key *media.Packet
	for i := range ps {
		p := &ps[i]
		if p.StreamIndex == videoStream && p.Key && p.PTSSec() <= t+1e-9 {
			key = p
		}
	}
	return key
}
