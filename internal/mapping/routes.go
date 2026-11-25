// Package mapping translates input track descriptions into deterministic
// output routes for a target container. Rules are matched by track number,
// language, and type in that priority order, and any unmapped track or
// incompatible codec fails before a byte of the final file is written.
package mapping

import (
	"fmt"
	"sort"
	"strings"

	"mediapipe/internal/media"
)

// Target names an output container family.
type Target string

const (
	TargetMatroska Target = "matroska"
	TargetWebM     Target = "webm"
	TargetFMP4     Target = "fmp4"
)

func (t Target) Valid() bool {
	switch t {
	case TargetMatroska, TargetWebM, TargetFMP4:
		return true
	}
	return false
}

// Rule selects input tracks. Empty selector fields match anything. Priority is
// TrackNumber > Language > Type; rules with equal specificity are ordered by
// their declared position so mapping is fully deterministic.
type Rule struct {
	TrackNumber uint64
	Language    string
	Type        media.TrackType
	// OutTrack is the output track number. Zero disables the rule; a disabled
	// rule is an error if it matches.
	OutTrack uint64
	// Drop removes the track from the output while still considering it
	// handled, which lets deliveries intentionally omit, say, subtitles.
	Drop bool
	// CodecOverride optionally rewrites the codec identifier for the target.
	CodecOverride media.Codec
}

// specificity orders rules: numeric track match beats language, which beats
// type, which beats a wildcard.
func (r Rule) specificity() int {
	score := 0
	if r.TrackNumber != 0 {
		score += 8
	}
	if r.Language != "" {
		score += 4
	}
	if r.Type != 0 {
		score += 2
	}
	return score
}

func (r Rule) matches(t media.Track) bool {
	if r.TrackNumber != 0 && r.TrackNumber != t.ID {
		return false
	}
	if r.Language != "" && !strings.EqualFold(r.Language, t.Language) {
		return false
	}
	if r.Type != 0 && r.Type != t.Type {
		return false
	}
	return true
}

// Route is a resolved input-to-output track assignment.
type Route struct {
	In            media.Track
	OutTrack      uint64
	OutCodec      media.Codec
	OutLanguage   string
	OutType       media.TrackType
	Dropped       bool
	RuleIndex     int
}

// Plan is the full mapping decision for one job.
type Plan struct {
	Target  Target
	Routes  []Route
	Kept    []Route
	Dropped []Route
}

// Validate checks a plan is internally consistent.
func (p *Plan) Validate() error {
	seen := make(map[uint64]bool, len(p.Kept))
	for _, r := range p.Kept {
		if r.OutTrack == 0 {
			return media.NewError("mapping.plan", media.ClassUnsupported,
				fmt.Errorf("track %d maps to output track 0", r.In.ID))
		}
		if seen[r.OutTrack] {
			return media.NewError("mapping.plan", media.ClassUnsupported,
				fmt.Errorf("output track %d assigned twice", r.OutTrack))
		}
		seen[r.OutTrack] = true
	}
	return nil
}

// supportedCodecs lists what each target can carry without transcoding.
var supportedCodecs = map[Target]map[media.Codec]bool{
	TargetMatroska: {
		media.CodecAVC: true, media.CodecVP8: true, media.CodecVP9: true,
		media.CodecMPEG2: true, media.CodecOpus: true, media.CodecAAC: true,
		media.CodecVorbis: true, media.CodecText: true,
	},
	TargetWebM: {
		media.CodecVP8: true, media.CodecVP9: true,
		media.CodecOpus: true, media.CodecVorbis: true,
		media.CodecText: true,
	},
	TargetFMP4: {
		media.CodecAVC: true, media.CodecOpus: true, media.CodecAAC: true,
	},
}

// SupportsCodec reports whether target can carry codec in a stream copy.
func SupportsCodec(target Target, codec media.Codec) bool {
	set, ok := supportedCodecs[target]
	if !ok {
		return false
	}
	return set[codec]
}

// Resolver applies rules to tracks.
type Resolver struct {
	Target Target
	Rules  []Rule
}

// NewResolver returns a resolver with rules sorted deterministically by
// specificity, then declaration order. The caller's slice is not modified.
func NewResolver(target Target, rules []Rule) *Resolver {
	cp := append([]Rule(nil), rules...)
	sort.SliceStable(cp, func(i, j int) bool {
		return cp[i].specificity() > cp[j].specificity()
	})
	return &Resolver{Target: target, Rules: cp}
}

// Resolve maps every input track or returns an error naming the first track
// that cannot be routed. Unsupported codecs are reported before any write.
func (r *Resolver) Resolve(tracks []media.Track) (*Plan, error) {
	if !r.Target.Valid() {
		return nil, media.NewError("mapping.target", media.ClassUnsupported,
			fmt.Errorf("unknown target %q", r.Target))
	}
	if len(tracks) == 0 {
		return nil, media.NewError("mapping.tracks", media.ClassFatal,
			fmt.Errorf("no input tracks"))
	}
	plan := &Plan{Target: r.Target}
	used := make(map[uint64]bool)
	for _, t := range tracks {
		matched := false
		for idx := range r.Rules {
			rule := r.Rules[idx]
			if !rule.matches(t) {
				continue
			}
			matched = true
			if rule.OutTrack == 0 && !rule.Drop {
				return nil, media.NewError("mapping.rule", media.ClassUnsupported,
					fmt.Errorf("rule %d matches track %d but assigns output track 0", idx, t.ID))
			}
			if used[rule.OutTrack] && !rule.Drop {
				return nil, media.NewError("mapping.rule", media.ClassUnsupported,
					fmt.Errorf("output track %d is claimed by more than one input track", rule.OutTrack))
			}
			route := Route{
				In:          t.Clone(),
				OutTrack:    rule.OutTrack,
				OutCodec:    t.Codec,
				OutLanguage: t.Language,
				OutType:     t.Type,
				Dropped:     rule.Drop,
				RuleIndex:   idx,
			}
			if rule.CodecOverride != "" {
				route.OutCodec = rule.CodecOverride
			}
			if rule.Drop {
				plan.Dropped = append(plan.Dropped, route)
				break
			}
			if !SupportsCodec(r.Target, route.OutCodec) {
				return nil, media.NewError("mapping.codec", media.ClassUnsupported,
					fmt.Errorf("track %d codec %q is not supported by target %s",
						t.ID, route.OutCodec, r.Target))
			}
			used[rule.OutTrack] = true
			plan.Routes = append(plan.Routes, route)
			plan.Kept = append(plan.Kept, route)
			break
		}
		if !matched {
			return nil, media.NewError("mapping.track", media.ClassUnsupported,
				fmt.Errorf("track %d (%s, %s) has no mapping rule",
					t.ID, t.Type, t.Language))
		}
	}
	if len(plan.Kept) == 0 {
		return nil, media.NewError("mapping.plan", media.ClassUnsupported,
			fmt.Errorf("mapping drops every input track"))
	}
	if err := plan.Validate(); err != nil {
		return nil, err
	}
	return plan, nil
}

// Remap returns the output track number for an input track, and whether it is
// kept.
func (p *Plan) Remap(inTrack uint64) (uint64, bool) {
	for _, r := range p.Kept {
		if r.In.ID == inTrack {
			return r.OutTrack, true
		}
	}
	return 0, false
}

// Lookup returns the kept route for an input track.
func (p *Plan) Lookup(inTrack uint64) (Route, bool) {
	for _, r := range p.Kept {
		if r.In.ID == inTrack {
			return r, true
		}
	}
	return Route{}, false
}

// OutputTracks returns the kept routes ordered by output track number.
func (p *Plan) OutputTracks() []Route {
	out := append([]Route(nil), p.Kept...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].OutTrack < out[j].OutTrack })
	return out
}

// DefaultRules returns rules for the broadcast demo: video and audio are kept
// as tracks 1 and 2, subtitles as track 3, ordered by input track number.
func DefaultRules() []Rule {
	return []Rule{
		{TrackNumber: 1, OutTrack: 1},
		{TrackNumber: 2, OutTrack: 2},
		{TrackNumber: 3, OutTrack: 3},
	}
}

// StrictRules maps every track by type so language changes do not break the
// delivery. It is used when an upstream manifest does not pin track numbers.
func StrictRules() []Rule {
	return []Rule{
		{Type: media.TrackVideo, OutTrack: 1},
		{Type: media.TrackAudio, OutTrack: 2},
		{Type: media.TrackSubtitle, OutTrack: 3},
	}
}
