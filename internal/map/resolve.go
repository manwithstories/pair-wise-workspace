// Package trackmap resolves which source track feeds each output target.
// Resolution order is explicit mapping, then track identifier, then media type.
// Duplicate targets, missing required targets and type mismatches fail the
// whole batch without emitting a partial result.
package trackmap

import (
	"fmt"
	"sort"

	"mediasync/internal/model"
)

// Target is one required output slot.
type Target struct {
	// Name is the output role, e.g. "program-video".
	Name string
	// Kind is the media type the target requires.
	Kind model.TrackKind
	// Required targets must be mapped or the batch fails.
	Required bool
	// Explicit, when set, names the source track ID that must feed this target.
	Explicit *uint32
}

// Selection is the resolved source for one target.
type Selection struct {
	Target Target
	Track  *model.Track
	Rule   string // "explicit", "track-id" or "type"
}

// Result is a full mapping decision with provenance for auditing.
type Result struct {
	Selections []Selection
	// Notes explain fallbacks, e.g. type-based selection among several tracks.
	Notes []string
}

// Options controls resolution.
type Options struct {
	// IDHints maps a target name to a preferred source track ID. It is used
	// when Target.Explicit is not set.
	IDHints map[string]uint32
}

// Resolve maps every target against the available tracks.
func Resolve(targets []Target, tracks []*model.Track, opt Options) (*Result, error) {
	if len(targets) == 0 {
		return nil, model.NewError(model.ErrParamInvalid, 0, 0, "no targets requested")
	}
	byID := make(map[uint32]*model.Track, len(tracks))
	for _, tr := range tracks {
		if tr == nil {
			continue
		}
		if _, dup := byID[tr.ID]; dup {
			return nil, model.NewError(model.ErrTrackMapDuplicate, tr.ID, 0, "source has duplicate track ID %d", tr.ID)
		}
		byID[tr.ID] = tr
	}

	usedTarget := map[string]bool{}
	usedTrack := map[uint32]string{}
	res := &Result{}
	for _, t := range targets {
		if t.Name == "" {
			return nil, model.NewError(model.ErrParamInvalid, 0, 0, "target with empty name")
		}
		if usedTarget[t.Name] {
			return nil, model.NewError(model.ErrTrackMapDuplicate, 0, 0, "duplicate target %q", t.Name)
		}
		usedTarget[t.Name] = true
		if !t.Kind.Valid() {
			return nil, model.NewError(model.ErrParamInvalid, 0, 0, "target %q has invalid kind %q", t.Name, t.Kind)
		}

		var chosen *model.Track
		rule := ""
		var wantID uint32
		haveID := false
		if t.Explicit != nil {
			wantID, haveID = *t.Explicit, true
		} else if opt.IDHints != nil {
			if id, ok := opt.IDHints[t.Name]; ok {
				wantID, haveID = id, true
			}
		}
		if haveID {
			tr, ok := byID[wantID]
			if !ok {
				return nil, model.NewError(model.ErrTrackMapMissing, wantID, 0,
					"target %q requires track %d, which the source does not contain", t.Name, wantID)
			}
			if tr.Kind != t.Kind {
				return nil, model.NewError(model.ErrTrackMapMismatch, tr.ID, 0,
					"target %q needs %s but track %d is %s", t.Name, t.Kind, tr.ID, tr.Kind)
			}
			chosen = tr
			if t.Explicit != nil {
				rule = "explicit"
			} else {
				rule = "track-id"
			}
		} else {
			cands := tracksOfKind(tracks, t.Kind)
			if len(cands) == 0 {
				if t.Required {
					return nil, model.NewError(model.ErrTrackMapMissing, 0, 0,
						"required target %q has no %s source track", t.Name, t.Kind)
				}
				continue
			}
			if len(cands) > 1 {
				res.Notes = append(res.Notes, fmt.Sprintf(
					"target %q: %d %s tracks available; choosing track %d (lowest ID)",
					t.Name, len(cands), t.Kind, cands[0].ID))
			}
			chosen = cands[0]
			rule = "type"
		}

		if prev, ok := usedTrack[chosen.ID]; ok {
			return nil, model.NewError(model.ErrTrackMapDuplicate, chosen.ID, 0,
				"track %d is already mapped to target %q; cannot also feed %q", chosen.ID, prev, t.Name)
		}
		usedTrack[chosen.ID] = t.Name
		res.Selections = append(res.Selections, Selection{Target: t, Track: chosen, Rule: rule})
	}

	for _, t := range targets {
		if !t.Required {
			continue
		}
		mapped := false
		for _, s := range res.Selections {
			if s.Target.Name == t.Name {
				mapped = true
				break
			}
		}
		if !mapped {
			return nil, model.NewError(model.ErrTrackMapMissing, 0, 0, "required target %q is unmapped", t.Name)
		}
	}
	return res, nil
}

// tracksOfKind returns tracks of the requested kind, sorted by ID.
func tracksOfKind(tracks []*model.Track, kind model.TrackKind) []*model.Track {
	var out []*model.Track
	for _, tr := range tracks {
		if tr != nil && tr.Kind == kind {
			out = append(out, tr)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// TargetsFromTracks derives the default two-target layout used by the demo:
// one video slot and one audio slot, regardless of how many tracks of each
// kind the source carries.
func TargetsFromTracks(tracks []*model.Track) []Target {
	var out []Target
	have := map[model.TrackKind]bool{}
	for _, tr := range tracks {
		if have[tr.Kind] {
			continue
		}
		switch tr.Kind {
		case model.KindVideo:
			out = append(out, Target{Name: "program-video", Kind: model.KindVideo, Required: true})
		case model.KindAudio:
			out = append(out, Target{Name: "program-audio", Kind: model.KindAudio, Required: true})
		default:
			continue
		}
		have[tr.Kind] = true
	}
	return out
}
