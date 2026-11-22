package manifest

import (
	"fmt"

	"mediaroute/internal/clock"
)

// Interval is one aligned segment window on the unified 90 kHz time base.
type Interval struct {
	Seq   int
	Start uint64
	End   uint64
}

// AudioSegment is an audio playlist entry expressed in sample counts.
type AudioSegment struct {
	Seq     int
	Samples uint64
	Rate    int
}

// AlignVideo builds contiguous video intervals from playlist segments.
func AlignVideo(p *Playlist) []Interval {
	out := make([]Interval, 0, len(p.Segments))
	var t uint64
	for _, s := range p.Segments {
		out = append(out, Interval{Seq: s.Seq, Start: t, End: t + s.DurationTicks})
		t += s.DurationTicks
	}
	return out
}

// AlignAudio converts audio sample counts to 90 kHz intervals and verifies
// the merged audio/video timelines stay within one video frame of drift.
func AlignAudio(segs []AudioSegment, video []Interval) ([]Interval, error) {
	out := make([]Interval, 0, len(segs))
	var t uint64
	for _, s := range segs {
		if s.Rate != clock.Rate48000 && s.Rate != clock.Rate44100 {
			return nil, &clock.Error{Op: "align-audio", Segment: s.Seq,
				Msg: fmt.Sprintf("unsupported sample rate %d", s.Rate)}
		}
		dur := clock.SamplesToTicks(s.Samples, s.Rate)
		out = append(out, Interval{Seq: s.Seq, Start: t, End: t + dur})
		t += dur
	}
	if len(video) > 0 && len(out) > 0 {
		vEnd := video[len(video)-1].End
		aEnd := out[len(out)-1].End
		var drift uint64
		if vEnd > aEnd {
			drift = vEnd - aEnd
		} else {
			drift = aEnd - vEnd
		}
		if drift > clock.TicksPerFrame30 {
			return nil, &clock.Error{Op: "align-audio", Segment: out[len(out)-1].Seq,
				StartTick: aEnd, EndTick: vEnd,
				Msg: fmt.Sprintf("audio/video drift %d ticks exceeds one frame", drift)}
		}
	}
	return out, nil
}

// Merge checks that the DASH segment grid agrees with the HLS-derived
// intervals and returns the unified intervals.
func Merge(m *MPD, video []Interval) ([]Interval, error) {
	if m.SegmentCount != 0 && m.SegmentCount != len(video) {
		return nil, &clock.Error{Op: "merge", Segment: -1,
			Msg: fmt.Sprintf("dash segment count %d != hls %d", m.SegmentCount, len(video))}
	}
	if m.SegmentDurationTicks != 0 && len(video) > 0 {
		for _, iv := range video {
			d := iv.End - iv.Start
			var diff uint64
			if d > m.SegmentDurationTicks {
				diff = d - m.SegmentDurationTicks
			} else {
				diff = m.SegmentDurationTicks - d
			}
			if diff > clock.TicksPerFrame30 {
				return nil, &clock.Error{Op: "merge", Segment: iv.Seq,
					StartTick: iv.Start, EndTick: iv.End,
					Msg: "segment duration disagrees with dash grid"}
			}
		}
	}
	return video, nil
}
