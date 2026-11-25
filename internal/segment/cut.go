// Package segment plans and emits cut points aligned to video keyframes and
// audio packet boundaries, then writes outputs atomically while preserving
// source color metadata. A color conflict is reported, never overwritten.
package segment

import (
	"sort"

	"medpipe/internal/clock"
	"medpipe/internal/mp4"
)

// Boundary is one aligned cut point expressed on every relevant timescale.
type Boundary struct {
	// Index is the segment ordinal starting at 0.
	Index int
	// StartMillis / EndMillis locate the window on the media timeline.
	StartMillis int
	EndMillis   int
	// VideoTick is the keyframe (sync sample) time on the video timescale.
	VideoTick clock.Tick
	// VideoByte is the absolute file offset of the chosen sync sample.
	VideoByte int64
	// AudioTick is the packet-boundary time on the audio timescale.
	AudioTick clock.Tick
	// AudioByte is the audio sample's absolute file offset.
	AudioByte int64
}

// Plan is the ordered set of aligned segments for one container.
type Plan struct {
	TargetMillis int
	Boundaries   []Boundary
}

// Planner resolves desired millisecond cut points to real sample boundaries.
type Planner struct {
	container *mp4.Container
	video     *mp4.Track
	audio     *mp4.Track
}

// NewPlanner selects the first video and audio tracks. A missing video track
// is allowed (audio-only input), but a missing audio track is an error since
// packet alignment cannot be guaranteed.
func NewPlanner(c *mp4.Container) (*Planner, error) {
	p := &Planner{container: c}
	for _, t := range c.Tracks {
		switch t.Kind {
		case mp4.KindVideo:
			if p.video == nil {
				p.video = t
			}
		case mp4.KindAudio:
			if p.audio == nil {
				p.audio = t
			}
		}
	}
	if p.audio == nil {
		return nil, clock.E(clock.KindCorrupt, "segment.plan",
			clock.WithDetail("no audio track for packet-boundary alignment"))
	}
	return p, nil
}

// Build walks the timeline in TargetMillis steps and snaps every boundary to
// the nearest preceding video keyframe and the nearest audio packet boundary.
func (p *Planner) Build(targetMillis int) (*Plan, error) {
	if targetMillis <= 0 {
		return nil, clock.E(clock.KindArgument, "segment.plan",
			clock.WithDetail("target duration must be positive, got %d", targetMillis))
	}
	durationMillis := int(p.container.Duration.Millis())
	if durationMillis <= 0 {
		return nil, clock.E(clock.KindCorrupt, "segment.plan",
			clock.WithDetail("container has zero duration"))
	}
	plan := &Plan{TargetMillis: targetMillis}
	idx := 0
	for start := 0; start < durationMillis; start += targetMillis {
		end := start + targetMillis
		if end > durationMillis {
			end = durationMillis
		}
		b := Boundary{Index: idx, StartMillis: start, EndMillis: end}
		if p.video != nil {
			sync, ok := p.syncAtOrBefore(start)
			if !ok {
				return nil, clock.E(clock.KindCorrupt, "segment.plan",
					clock.WithDetail("no keyframe at or before %d ms", start))
			}
			b.VideoTick = sync.DTS
			b.VideoByte = sync.ByteOffset
		}
		audio, ok := p.audioPacketAtOrBefore(start)
		if !ok {
			return nil, clock.E(clock.KindCorrupt, "segment.plan",
				clock.WithDetail("no audio packet at or before %d ms", start))
		}
		b.AudioTick = audio.DTS
		b.AudioByte = audio.ByteOffset
		plan.Boundaries = append(plan.Boundaries, b)
		idx++
	}
	return plan, nil
}

// syncAtOrBefore returns the latest sync sample whose DTS is at or before the
// requested millisecond. Samples are already in DTS order from the parser.
func (p *Planner) syncAtOrBefore(millis int) (mp4.SamplePoint, bool) {
	target := millisToTick(millis, p.video.Timescale)
	var best mp4.SamplePoint
	found := false
	for _, s := range p.video.Samples {
		if !s.IsSync {
			continue
		}
		cmp, err := s.DTS.Cmp(target)
		if err != nil {
			continue
		}
		if cmp <= 0 {
			best, found = s, true
		} else {
			break
		}
	}
	return best, found
}

// audioPacketAtOrBefore returns the audio sample whose packet boundary is at
// or before the requested millisecond.
func (p *Planner) audioPacketAtOrBefore(millis int) (mp4.SamplePoint, bool) {
	target := millisToTick(millis, p.audio.Timescale)
	var best mp4.SamplePoint
	found := false
	for _, s := range p.audio.Samples {
		cmp, err := s.DTS.Cmp(target)
		if err != nil {
			continue
		}
		if cmp <= 0 {
			best, found = s, true
		} else {
			break
		}
	}
	return best, found
}

func millisToTick(millis int, timescale uint32) clock.Tick {
	// Exact rational conversion: ticks = millis * timescale / 1000.
	return clock.MustNew(uint64(millis)*uint64(timescale)/1000, timescale)
}

// SortedOffsets exposes unique aligned byte offsets for tests/verification.
func (pl *Plan) SortedOffsets() []int64 {
	seen := map[int64]bool{}
	var out []int64
	for _, b := range pl.Boundaries {
		if b.VideoByte > 0 && !seen[b.VideoByte] {
			seen[b.VideoByte] = true
			out = append(out, b.VideoByte)
		}
		if b.AudioByte > 0 && !seen[b.AudioByte] {
			seen[b.AudioByte] = true
			out = append(out, b.AudioByte)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
