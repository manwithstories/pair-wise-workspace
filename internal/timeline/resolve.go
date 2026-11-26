package timeline

import (
	"fmt"
	"sort"

	"pairflow/internal/media"
)

// Segment is one resolved span on a track after cuts and transitions are
// applied. The portion [End-DissolveOut, End) cross-fades into the next clip;
// the follower carries the same duration as DissolveIn so both sides describe
// one continuous blend window.
type Segment struct {
	TrackID     string
	ClipID      string
	Start       media.MediaTime
	End         media.MediaTime
	DissolveOut media.MediaTime
	DissolveIn  media.MediaTime
	Audio       *AudioSpec
	Video       *VideoSpec
}

// Duration returns End - Start.
func (s Segment) Duration() (media.MediaTime, *media.Error) {
	return s.End.Since(s.Start)
}

// Resolved is a track's validated edit map.
type Resolved struct {
	TrackID  string
	Kind     string
	Segments []Segment
}

// clipSpan is a clip with its parsed bounds, used during resolution.
type clipSpan struct {
	clip  *Clip
	index int
	start media.MediaTime
	stop  media.MediaTime
}

func (s clipSpan) duration() media.MediaTime {
	d, _ := s.stop.Since(s.start) // start<stop was enforced during parsing
	return d
}

// Resolve validates and flattens a timeline. It rejects reversed or
// out-of-bounds cuts, unknown transitions and any boundary that overflows the
// timeline duration.
func Resolve(tl *RawTimeline) ([]Resolved, *media.Error) {
	end, err := tl.Duration.Media()
	if err != nil {
		return nil, err.WithField("duration")
	}
	if end.Num == 0 {
		return nil, media.Input("timeline.empty", "timeline duration must be positive").WithField("duration")
	}
	out := make([]Resolved, 0, len(tl.Tracks))
	for ti := range tl.Tracks {
		tr := &tl.Tracks[ti]
		rs, err := resolveTrack(tr, end)
		if err != nil {
			return nil, err.WithPath(fmt.Sprintf("tracks[%s]", tr.ID))
		}
		out = append(out, rs)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].TrackID < out[j].TrackID })
	return out, nil
}

func resolveTrack(tr *Track, timelineEnd media.MediaTime) (Resolved, *media.Error) {
	spans := make([]clipSpan, 0, len(tr.Clips))
	for ci := range tr.Clips {
		c := &tr.Clips[ci]
		st, err := c.Start.Media()
		if err != nil {
			return Resolved{}, err.WithField(fieldPath(tr, ci, "start"))
		}
		en, err := c.End.Media()
		if err != nil {
			return Resolved{}, err.WithField(fieldPath(tr, ci, "end"))
		}
		if !st.Less(en) {
			return Resolved{}, media.Input("timeline.reversed_cut",
				"clip %q starts at %s but ends at %s", c.ID, st, en).
				WithField(fieldPath(tr, ci, "end"))
		}
		if en.Cmp(timelineEnd) > 0 {
			return Resolved{}, media.Input("timeline.out_of_bounds",
				"clip %q ends at %s beyond timeline %s", c.ID, en, timelineEnd).
				WithField(fieldPath(tr, ci, "end"))
		}
		spans = append(spans, clipSpan{clip: c, index: ci, start: st, stop: en})
	}
	// Clips must abut or leave gaps; bare overlap needs a declared transition.
	for i := 1; i < len(spans); i++ {
		if spans[i].start.Cmp(spans[i-1].stop) < 0 {
			return Resolved{}, media.Input("timeline.clip_overlap",
				"clip %q starts %s before clip %q ends %s without a transition",
				spans[i].clip.ID, spans[i].start, spans[i-1].clip.ID, spans[i-1].stop).
				WithField(fieldPath(tr, spans[i].index, "start"))
		}
	}

	type pair struct{ from, to string }
	trans := map[pair]Transition{}
	for di := range tr.Transitions {
		d := &tr.Transitions[di]
		f := fmt.Sprintf("tracks[%s].transitions[%d]", tr.ID, di)
		if d.Kind != "cross_dissolve" {
			return Resolved{}, media.Input("timeline.unknown_transition",
				"transition kind %q is unknown (only cross_dissolve is supported)", d.Kind).
				WithField(f + ".kind")
		}
		from := spanByID(spans, d.FromClip)
		to := spanByID(spans, d.ToClip)
		if from == nil || to == nil {
			return Resolved{}, media.Input("timeline.transition_clip",
				"transition references unknown clip %q -> %q", d.FromClip, d.ToClip).
				WithField(f)
		}
		if from.clip.ID == to.clip.ID {
			return Resolved{}, media.Input("timeline.transition_self",
				"transition joins clip %q to itself", d.FromClip).WithField(f)
		}
		if to.index != from.index+1 {
			return Resolved{}, media.Input("timeline.transition_order",
				"transition %q -> %q does not join consecutive clips", d.FromClip, d.ToClip).
				WithField(f)
		}
		dur, err := d.Duration.Media()
		if err != nil {
			return Resolved{}, err.WithField(f + ".duration")
		}
		if dur.Num == 0 {
			return Resolved{}, media.Input("timeline.transition_zero",
				"transition %q has zero duration", d.Kind).WithField(f + ".duration")
		}
		if dur.Cmp(from.duration()) > 0 {
			return Resolved{}, media.Input("timeline.transition_oob",
				"transition %s exceeds clip %q duration %s", dur, from.clip.ID, from.duration()).
				WithField(f + ".duration")
		}
		if dur.Cmp(to.duration()) > 0 {
			return Resolved{}, media.Input("timeline.transition_oob",
				"transition %s exceeds clip %q duration %s", dur, to.clip.ID, to.duration()).
				WithField(f + ".duration")
		}
		gap, gerr := to.start.Since(from.stop)
		if gerr == nil && gap.Num > 0 {
			return Resolved{}, media.Input("timeline.transition_gap",
				"dissolve %q -> %q requires abutting clips, gap is %s", d.FromClip, d.ToClip, gap).
				WithField(f)
		}
		if _, dup := trans[pair{d.FromClip, d.ToClip}]; dup {
			return Resolved{}, media.Input("timeline.transition_duplicate",
				"duplicate transition %q -> %q", d.FromClip, d.ToClip).WithField(f)
		}
		trans[pair{d.FromClip, d.ToClip}] = *d
	}

	segs := make([]Segment, 0, len(spans))
	for i := range spans {
		seg := Segment{
			TrackID: tr.ID,
			ClipID:  spans[i].clip.ID,
			Start:   spans[i].start,
			End:     spans[i].stop,
			Audio:   spans[i].clip.Audio,
			Video:   spans[i].clip.Video,
		}
		if i+1 < len(spans) {
			if d, ok := trans[pair{spans[i].clip.ID, spans[i+1].clip.ID}]; ok {
				dur, _ := d.Duration.Media() // already validated above
				// The dissolve occupies the tail of this clip; the boundary
				// shifts to the follower's start so no time is lost.
				seg.End = spans[i+1].start
				seg.DissolveOut = dur
			}
		}
		segs = append(segs, seg)
	}
	for i := 0; i+1 < len(segs); i++ {
		if segs[i].DissolveOut.Num > 0 {
			segs[i+1].DissolveIn = segs[i].DissolveOut
		}
	}
	return Resolved{TrackID: tr.ID, Kind: tr.Kind, Segments: segs}, nil
}

func spanByID(spans []clipSpan, id string) *clipSpan {
	for i := range spans {
		if spans[i].clip.ID == id {
			return &spans[i]
		}
	}
	return nil
}

func fieldPath(tr *Track, ci int, name string) string {
	return fmt.Sprintf("tracks[%s].clips[%d].%s", tr.ID, ci, name)
}
