// Package timeline composes video samples, audio samples and subtitle
// cues onto one rational timeline. All entries share the clock.Rational
// basis and the clock.SourceError structure.
package timeline

import (
	"sort"

	"pairflow/media/internal/clock"
	"pairflow/media/internal/mp4"
	"pairflow/media/internal/subtitle"
)

// Kind identifies the media type of a timeline entry.
type Kind string

const (
	KindVideo    Kind = "video"
	KindAudio    Kind = "audio"
	KindSubtitle Kind = "subtitle"
)

// ConflictPolicy decides what happens when subtitle cues overlap on
// the composed timeline.
type ConflictPolicy string

const (
	// ConflictLastWins keeps the later-starting cue and trims the
	// earlier one so the timeline never double-books a moment.
	ConflictLastWins ConflictPolicy = "last-wins"
	// ConflictKeepBoth preserves both cues and records the overlap.
	ConflictKeepBoth ConflictPolicy = "keep-both"
	// ConflictReject drops the later cue and records a conflict.
	ConflictReject ConflictPolicy = "reject"
)

// ParseConflictPolicy validates the CLI value.
func ParseConflictPolicy(s string) (ConflictPolicy, error) {
	switch ConflictPolicy(s) {
	case ConflictLastWins, ConflictKeepBoth, ConflictReject:
		return ConflictPolicy(s), nil
	case "":
		return ConflictLastWins, nil
	}
	return "", clock.Errorf("timeline", "bad_conflict_policy", -1,
		"unknown subtitle conflict policy %q", s)
}

// TrackInput is everything one track contributes.
type TrackInput struct {
	Name string
	Kind Kind
	MP4  *mp4.TrackIndex
	Sub  *subtitle.Document
	// Priority orders subtitle tracks; higher wins conflicts.
	Priority int
}

// Entry is one composed item. For media samples Start/End span the
// sample; for subtitles they span the cue.
type Entry struct {
	Kind     Kind           `json:"kind"`
	Track    string         `json:"track"`
	Priority int            `json:"priority"`
	Start    clock.Rational `json:"start"`
	End      clock.Rational `json:"end"`
	Frame    int            `json:"frame,omitempty"`
	Text     string         `json:"text,omitempty"`
	Sample   *mp4.Sample    `json:"sample,omitempty"`
	Offset   int64          `json:"offset,omitempty"`
	// Trimmed is set when a conflict policy shortened this entry.
	Trimmed bool `json:"trimmed,omitempty"`
	// ConflictID links entries involved in the same overlap decision.
	ConflictID int `json:"conflict_id,omitempty"`
}

// Duration is End-Start, clamped at zero.
func (e Entry) Duration() clock.Rational {
	if e.End.Cmp(e.Start) <= 0 {
		return clock.Zero()
	}
	return e.End.Sub(e.Start)
}

// Timeline is the composed result.
type Timeline struct {
	Window        Window               `json:"window"`
	Policy        ConflictPolicy       `json:"conflict_policy"`
	Entries       []Entry              `json:"entries"`
	Conflicts     []Conflict           `json:"conflicts"`
	Errors        []*clock.SourceError `json:"errors"`
	SubtitleCount int                  `json:"subtitle_count"`
	VideoCount    int                  `json:"video_count"`
	AudioCount    int                  `json:"audio_count"`
	FrameDuration clock.Rational       `json:"frame_duration"`
}

// Window is a half-open [Start,End) interval on the shared time axis.
type Window struct {
	Start clock.Rational `json:"start"`
	End   clock.Rational `json:"end"`
}

// Contains reports Start <= t < End.
func (w Window) Contains(t clock.Rational) bool {
	return w.Start.Cmp(t) <= 0 && t.Cmp(w.End) < 0
}

// Intersect returns the overlap of two windows and whether it is
// non-empty.
func (w Window) Intersect(o Window) (Window, bool) {
	start := w.Start
	if o.Start.Cmp(start) > 0 {
		start = o.Start
	}
	end := w.End
	if o.End.Cmp(end) < 0 {
		end = o.End
	}
	if end.Cmp(start) <= 0 {
		return Window{}, false
	}
	return Window{Start: start, End: end}, true
}

// Conflict records one subtitle overlap decision.
type Conflict struct {
	ID       int            `json:"id"`
	Track    string         `json:"track"`
	Earlier  int            `json:"earlier_index"`
	Later    int            `json:"later_index"`
	Overlap  clock.Rational `json:"overlap"`
	Start    clock.Rational `json:"start"`
	End      clock.Rational `json:"end"`
	Resolved string         `json:"resolution"`
	Priority int            `json:"priority"`
}

// Options configures composition.
type Options struct {
	Window        Window
	Policy        ConflictPolicy
	FrameDuration clock.Rational
	// MaxEntries bounds memory. Zero means no explicit bound beyond the
	// caller's 512 MiB budget.
	MaxEntries int
}

// Compose merges tracks into one ordered timeline.
func Compose(tracks []TrackInput, opts Options) *Timeline {
	tl := &Timeline{Window: opts.Window, Policy: opts.Policy, FrameDuration: opts.FrameDuration}
	if tl.Policy == "" {
		tl.Policy = ConflictLastWins
	}
	for _, tr := range tracks {
		switch tr.Kind {
		case KindVideo, KindAudio:
			composeMedia(tl, tr, opts)
		case KindSubtitle:
			composeSubtitle(tl, tr, opts)
		}
	}
	resolveCrossTrackSubtitles(tl, opts)
	dropCollapsedSubtitles(tl)
	sort.SliceStable(tl.Entries, func(i, j int) bool {
		if c := tl.Entries[i].Start.Cmp(tl.Entries[j].Start); c != 0 {
			return c < 0
		}
		if tl.Entries[i].Kind != tl.Entries[j].Kind {
			return tl.Entries[i].Kind < tl.Entries[j].Kind
		}
		return tl.Entries[i].Frame < tl.Entries[j].Frame
	})
	return tl
}

func composeMedia(tl *Timeline, tr TrackInput, opts Options) {
	if tr.MP4 == nil {
		return
	}
	samples := tr.MP4.Samples
	for i := range samples {
		s := samples[i]
		end := s.DTS.Add(opts.FrameDuration)
		if i+1 < len(samples) {
			end = samples[i+1].DTS
		}
		if end.Cmp(s.DTS) <= 0 {
			end = s.DTS
		}
		entry := Entry{
			Kind: tr.Kind, Track: tr.Name, Priority: tr.Priority,
			Start: s.DTS, End: end, Frame: s.Number, Sample: &samples[i],
			Offset: int64(s.Offset),
		}
		e, ok := clip(entry, opts.Window)
		if !ok {
			continue
		}
		tl.Entries = append(tl.Entries, e)
		if tr.Kind == KindVideo {
			tl.VideoCount++
		} else {
			tl.AudioCount++
		}
		if opts.MaxEntries > 0 && len(tl.Entries) >= opts.MaxEntries {
			tl.Errors = append(tl.Errors, clock.Errorf("timeline", "entry_limit", -1,
				"stopped after %d entries (limit %d)", len(tl.Entries), opts.MaxEntries))
			return
		}
	}
	for _, e := range tr.MP4.Errors {
		tl.Errors = append(tl.Errors, e)
	}
}

// clip trims an entry to the window. Fully outside entries are dropped.
func clip(e Entry, w Window) (Entry, bool) {
	if w.End.Cmp(w.Start) <= 0 {
		return e, true // no window configured
	}
	if e.End.Cmp(w.Start) <= 0 || w.End.Cmp(e.Start) <= 0 {
		return e, false
	}
	if e.Start.Cmp(w.Start) < 0 {
		e.Start = w.Start
	}
	if w.End.Cmp(e.End) < 0 {
		e.End = w.End
	}
	return e, true
}

// composeSubtitle applies the conflict policy between cues of the same
// track. Cross-track overlaps are resolved afterwards by priority.
func composeSubtitle(tl *Timeline, tr TrackInput, opts Options) {
	if tr.Sub == nil {
		return
	}
	cues := append([]subtitle.Cue(nil), tr.Sub.Cues...)
	sort.SliceStable(cues, func(i, j int) bool {
		if c := cues[i].Start.Cmp(cues[j].Start); c != 0 {
			return c < 0
		}
		return cues[i].End.Cmp(cues[j].End) < 0
	})
	// Each kept entry remembers the source cue ordinal for reporting.
	type keptEntry struct {
		entry Entry
		cue   int
	}
	var kept []keptEntry
	conflictSeq := len(tl.Conflicts)
	for cueSeq, c := range cues {
		entry := Entry{
			Kind: KindSubtitle, Track: tr.Name, Priority: tr.Priority,
			Start: c.Start, End: c.End, Text: c.Text, Offset: c.Offset,
		}
		entry, ok := clip(entry, opts.Window)
		if !ok {
			continue
		}
		// Find an overlap with any already-kept cue of this track.
		idx := -1
		for i := range kept {
			k := kept[i].entry
			if k.End.Cmp(entry.Start) > 0 && entry.Start.Cmp(k.Start) >= 0 {
				idx = i
			}
		}
		if idx < 0 {
			kept = append(kept, keptEntry{entry: entry, cue: cueSeq})
			continue
		}
		earlier := kept[idx].entry
		earlierCue := kept[idx].cue
		// The overlap runs from the later cue's start to whichever cue
		// ends first.
		overlapEnd := earlier.End
		if entry.End.Cmp(overlapEnd) < 0 {
			overlapEnd = entry.End
		}
		overlap := Window{Start: entry.Start, End: overlapEnd}
		conflictSeq++
		cf := Conflict{
			ID: conflictSeq, Track: tr.Name,
			Earlier: earlierCue, Later: cueSeq,
			Overlap: overlap.End.Sub(overlap.Start),
			Start:   overlap.Start, End: overlap.End,
			Priority: tr.Priority,
		}
		switch tl.Policy {
		case ConflictKeepBoth:
			cf.Resolved = "kept-both"
			kept = append(kept, keptEntry{entry: entry, cue: cueSeq})
		case ConflictReject:
			cf.Resolved = "rejected-later"
		default: // last-wins
			cf.Resolved = "trimmed-earlier"
			earlier.End = entry.Start
			earlier.Trimmed = true
			kept[idx].entry = earlier
			kept = append(kept, keptEntry{entry: entry, cue: cueSeq})
		}
		tl.Conflicts = append(tl.Conflicts, cf)
	}
	for _, k := range kept {
		tl.Entries = append(tl.Entries, k.entry)
		tl.SubtitleCount++
	}
	for _, e := range tr.Sub.Errors {
		tl.Errors = append(tl.Errors, e)
	}
}

// SubtitleBounds returns the earliest start and latest end across all
// subtitle entries.
func (t *Timeline) SubtitleBounds() (clock.Rational, clock.Rational, bool) {
	var lo, hi clock.Rational
	found := false
	for _, e := range t.Entries {
		if e.Kind != KindSubtitle {
			continue
		}
		if !found {
			lo, hi, found = e.Start, e.End, true
			continue
		}
		if e.Start.Cmp(lo) < 0 {
			lo = e.Start
		}
		if hi.Cmp(e.End) < 0 {
			hi = e.End
		}
	}
	return lo, hi, found
}

// resolveCrossTrackSubtitles applies track priority to subtitle entries
// from different tracks that occupy the same moment. The higher
// priority entry keeps its span; the lower priority entry is trimmed,
// dropped or left alone according to the policy. Within-track conflicts
// were already handled during composition.
func resolveCrossTrackSubtitles(tl *Timeline, opts Options) {
	subIdx := make([]int, 0, len(tl.Entries))
	for i := range tl.Entries {
		if tl.Entries[i].Kind == KindSubtitle {
			subIdx = append(subIdx, i)
		}
	}
	sort.SliceStable(subIdx, func(a, b int) bool {
		ea, eb := tl.Entries[subIdx[a]], tl.Entries[subIdx[b]]
		if c := ea.Start.Cmp(eb.Start); c != 0 {
			return c < 0
		}
		return ea.Priority > eb.Priority
	})
	resolved := make(map[[2]int]bool)
	for ai := 0; ai < len(subIdx); ai++ {
		i := subIdx[ai]
		for bi := ai + 1; bi < len(subIdx); bi++ {
			j := subIdx[bi]
			a, b := tl.Entries[i], tl.Entries[j]
			if b.Start.Cmp(a.End) >= 0 {
				break // sorted by start: no later entry can overlap
			}
			if a.Track == b.Track || a.Priority == b.Priority {
				continue
			}
			key := [2]int{i, j}
			if i > j {
				key = [2]int{j, i}
			}
			if resolved[key] {
				continue
			}
			resolved[key] = true
			winner, loser := i, j
			if b.Priority > a.Priority {
				winner, loser = j, i
			}
			w, l := tl.Entries[winner], tl.Entries[loser]
			overlapEnd := w.End
			if l.End.Cmp(overlapEnd) < 0 {
				overlapEnd = l.End
			}
			overlapStart := l.Start
			if w.Start.Cmp(overlapStart) > 0 {
				overlapStart = w.Start
			}
			if overlapEnd.Cmp(overlapStart) <= 0 {
				continue
			}
			cf := Conflict{
				ID: len(tl.Conflicts) + 1, Track: l.Track,
				Earlier: winner, Later: loser,
				Overlap: overlapEnd.Sub(overlapStart),
				Start:   overlapStart, End: overlapEnd,
				Priority: w.Priority,
			}
			switch tl.Policy {
			case ConflictKeepBoth:
				cf.Resolved = "kept-both"
			case ConflictReject:
				cf.Resolved = "rejected-lower-priority"
				le := tl.Entries[loser]
				le.End = le.Start
				le.Trimmed = true
				tl.Entries[loser] = le
			default: // last-wins trims the lower priority cue
				cf.Resolved = "trimmed-lower-priority"
				le := tl.Entries[loser]
				switch {
				case l.Start.Cmp(w.Start) >= 0:
					// Loser begins inside the winner: keep only its tail.
					le.Start = w.End
					if le.End.Cmp(le.Start) < 0 {
						le.End = le.Start
					}
				case l.End.Cmp(w.End) > 0:
					// Loser starts before the winner and ends after it:
					// keep the leading part only.
					le.End = w.Start
					if le.End.Cmp(le.Start) < 0 {
						le.End = le.Start
					}
				default:
					// Loser fully covered: collapse it.
					le.End = le.Start
				}
				tl.Entries[loser] = le
				tl.Entries[loser].Trimmed = true
			}
			tl.Conflicts = append(tl.Conflicts, cf)
		}
	}
}

// dropCollapsedSubtitles removes subtitle entries that a conflict policy
// reduced to an empty span, and recomputes the subtitle count so the
// summary never counts a cue that covers no moment.
//
// Two kinds of collapse are distinguished: within-track policies never
// trim a cue to nothing, so an empty span there means the source cue was
// zero-length and is preserved; cross-track priority trimming does set
// Trimmed for a cue it empties, and those are dropped.
func dropCollapsedSubtitles(tl *Timeline) {
	out := tl.Entries[:0]
	count := 0
	for _, e := range tl.Entries {
		if e.Kind == KindSubtitle {
			if e.Trimmed && e.End.Cmp(e.Start) <= 0 {
				continue
			}
			count++
		}
		out = append(out, e)
	}
	tl.Entries = out
	tl.SubtitleCount = count
}
