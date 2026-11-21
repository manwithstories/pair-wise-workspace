// Package mapping resolves source tracks to delivery roles by content
// fingerprint and adjudicates display metadata in a fixed precedence order:
// container, then track, then explicit policy. Every original value and the
// reason for the decision are retained for audit.
package mapping

import (
	"fmt"
	"sort"

	"mediameld/internal/model"
)

// Role names a desired stream in the delivery timeline.
type Role string

const (
	RoleMainVideo Role = "video-main"
	RoleMainAudio Role = "audio-main"
	RoleAuxAudio  Role = "audio-aux"
)

// Entry binds a role to a track fingerprint. Fingerprints (not file order or
// array index) identify inputs, so reordering inputs cannot silently rebind a
// track to the wrong role.
type Entry struct {
	Role        Role
	Fingerprint string
	Input       string // informational only, never used for matching
}

// Requirement declares the roles a delivery must contain.
type Requirement struct {
	Video []Role
	Audio []Role
}

// DefaultRequirement requires exactly one main video and one main audio.
func DefaultRequirement() Requirement {
	return Requirement{Video: []Role{RoleMainVideo}, Audio: []Role{RoleMainAudio}}
}

// Candidate is a track and the input it came from.
type Candidate struct {
	Input       string
	Track       *model.Track
	Fingerprint string
}

// BuildCandidates fingerprints every track up front.
func BuildCandidates(inputs map[string][]*model.Track) []Candidate {
	var out []Candidate
	for path, tracks := range inputs {
		for _, t := range tracks {
			out = append(out, Candidate{Input: path, Track: t, Fingerprint: t.Fingerprint()})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Fingerprint < out[j].Fingerprint })
	return out
}

// Resolution is the outcome for one role.
type Resolution struct {
	Role        Role
	Fingerprint string
	Track       *model.Track
	Decision    DisplayDecision
}

// DisplayDecision records the adjudication trail for one track.
type DisplayDecision struct {
	Rotation int
	Crop     model.Crop
	Color    model.ColorDesc
	Reasons  []Reason
}

// Reason is one adjudication step with the original value it displaced.
type Reason struct {
	Field    string
	Source   string // "container", "track" or "policy"
	Original string
	Chosen   string
	Why      string
}

// Policy is the explicit, highest-precedence operator override. Nil fields
// mean "no override for this property".
type Policy struct {
	Rotation *int
	Crop     *model.Crop
	Color    *model.ColorDesc
}

// Resolve binds roles to tracks and adjudicates display metadata. Failures
// always name the conflicting object (role or fingerprint).
func Resolve(entries []Entry, candidates []Candidate, policy Policy, req Requirement) ([]Resolution, error) {
	byFP := make(map[string][]Candidate, len(candidates))
	for _, c := range candidates {
		byFP[c.Fingerprint] = append(byFP[c.Fingerprint], c)
	}
	// A mapping entry must not reference the same fingerprint twice, and two
	// roles must not claim one fingerprint unless the other is an aux alias.
	seenRole := map[Role]Entry{}
	seenFP := map[string]Role{}
	var out []Resolution
	for _, e := range entries {
		if prev, ok := seenRole[e.Role]; ok {
			return nil, model.NewError(model.CodeTrackDuplicate,
				"role %q mapped twice: %s and %s", e.Role, prev.Fingerprint, e.Fingerprint)
		}
		if prev, ok := seenFP[e.Fingerprint]; ok {
			return nil, model.NewError(model.CodeTrackDuplicate,
				"fingerprint %s mapped to both roles %q and %q", e.Fingerprint, prev, e.Role)
		}
		matches := byFP[e.Fingerprint]
		if len(matches) == 0 {
			return nil, model.NewError(model.CodeTrackMissing,
				"role %q references fingerprint %s which matches no input track", e.Role, e.Fingerprint)
		}
		if len(matches) > 1 {
			return nil, model.NewError(model.CodeFingerprint,
				"fingerprint %s is ambiguous across %d inputs", e.Fingerprint, len(matches))
		}
		seenRole[e.Role] = e
		seenFP[e.Fingerprint] = e.Role
		t := matches[0].Track
		out = append(out, Resolution{
			Role:        e.Role,
			Fingerprint: e.Fingerprint,
			Track:       t,
			Decision:    adjudicate(t, policy),
		})
	}
	// Required roles must all be present, split by media kind so the error
	// distinguishes a missing video from a missing audio.
	present := map[Role]bool{}
	for _, r := range out {
		present[r.Role] = true
	}
	for _, r := range req.Video {
		if !present[r] {
			return nil, model.NewError(model.CodeTrackMissing, "required video role %q is missing", r)
		}
	}
	for _, r := range req.Audio {
		if !present[r] {
			return nil, model.NewError(model.CodeTrackMissing, "required audio role %q is missing", r)
		}
	}
	if err := validateComposition(out); err != nil {
		return nil, err
	}
	return out, nil
}

// adjudicate applies container -> track -> policy precedence per property.
func adjudicate(t *model.Track, policy Policy) DisplayDecision {
	d := DisplayDecision{}
	// Rotation: container tkhd matrix is the base, track descriptor overrides.
	d.Rotation = t.ContainerRotation
	if t.TrackRotation != 0 || t.ContainerRotation != 0 {
		d.Reasons = append(d.Reasons, Reason{
			Field: "rotation", Source: "container",
			Original: fmt.Sprint(t.ContainerRotation), Chosen: fmt.Sprint(t.ContainerRotation),
			Why: "container rotation from tkhd matrix",
		})
	}
	if t.TrackRotation != 0 {
		d.Reasons = append(d.Reasons, Reason{
			Field: "rotation", Source: "track",
			Original: fmt.Sprint(t.ContainerRotation), Chosen: fmt.Sprint(t.TrackRotation),
			Why: "track descriptor overrides container",
		})
		d.Rotation = t.TrackRotation
	}
	if policy.Rotation != nil {
		d.Reasons = append(d.Reasons, Reason{
			Field: "rotation", Source: "policy",
			Original: fmt.Sprint(d.Rotation), Chosen: fmt.Sprint(*policy.Rotation),
			Why: "explicit policy override",
		})
		d.Rotation = normalizeRotation(*policy.Rotation)
	}

	// Crop: container clap, then track crop, then policy.
	d.Crop = t.ContainerCrop
	if t.ContainerCrop.Valid {
		d.Reasons = append(d.Reasons, Reason{
			Field: "crop", Source: "container", Original: cropString(t.ContainerCrop),
			Chosen: cropString(t.ContainerCrop), Why: "clean aperture from container",
		})
	}
	if t.TrackCrop.Valid {
		d.Reasons = append(d.Reasons, Reason{
			Field: "crop", Source: "track", Original: cropString(t.ContainerCrop),
			Chosen: cropString(t.TrackCrop), Why: "track descriptor overrides container aperture",
		})
		d.Crop = t.TrackCrop
	}
	if policy.Crop != nil {
		d.Reasons = append(d.Reasons, Reason{
			Field: "crop", Source: "policy", Original: cropString(d.Crop),
			Chosen: cropString(*policy.Crop), Why: "explicit policy override",
		})
		d.Crop = *policy.Crop
	}

	// Colour: unknown values never overwrite a known layer; an unknown
	// highest layer still yields "unknown" rather than a guessed value.
	d.Color = t.ContainerColor
	if t.ContainerColor.Primaries != "" {
		d.Reasons = append(d.Reasons, Reason{
			Field: "color", Source: "container", Original: colorString(t.ContainerColor),
			Chosen: colorString(t.ContainerColor), Why: "container colour description",
		})
	}
	if t.TrackColor.Primaries != "" && t.TrackColor.Primaries != "unknown" {
		d.Reasons = append(d.Reasons, Reason{
			Field: "color", Source: "track", Original: colorString(t.ContainerColor),
			Chosen: colorString(t.TrackColor), Why: "track descriptor overrides container",
		})
		d.Color = t.TrackColor
	} else if t.TrackColor.Primaries == "unknown" && (d.Color.Primaries == "" || d.Color.Primaries == "unknown") {
		d.Color = model.UnknownColor(t.TrackColor.OriginalTag)
	}
	if policy.Color != nil {
		d.Reasons = append(d.Reasons, Reason{
			Field: "color", Source: "policy", Original: colorString(d.Color),
			Chosen: colorString(*policy.Color), Why: "explicit policy override",
		})
		d.Color = *policy.Color
	}
	return d
}

// validateComposition rejects a delivery whose geometry cannot be satisfied,
// naming the offending track.
func validateComposition(res []Resolution) error {
	for _, r := range res {
		w, h := r.Track.CodedWidth, r.Track.CodedHeight
		if w == 0 || h == 0 {
			continue
		}
		if err := r.Decision.Crop.Validate(w, h); err != nil {
			return model.Tag(err, r.Track.ID)
		}
	}
	return nil
}

func normalizeRotation(deg int) int {
	d := ((deg % 360) + 360) % 360
	switch d {
	case 90, 180, 270:
		return d
	default:
		return 0
	}
}

func cropString(c model.Crop) string {
	if !c.Valid {
		return "none"
	}
	return fmt.Sprintf("%dx%d+%d+%d", c.W, c.H, c.X, c.Y)
}

func colorString(c model.ColorDesc) string {
	return fmt.Sprintf("%s/%s/%s/%s", c.Primaries, c.Transfer, c.Matrix, c.FullRange)
}
