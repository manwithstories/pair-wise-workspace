// Package mapping validates and applies explicit track mappings: which input
// track feeds which target slot. It rejects duplicate targets, missing
// required tracks, and incompatible codec parameters before any output is
// produced, so a bad manifest can never reach the remux stage.
package mapping

import (
	"fmt"
	"sort"

	"mediarun/internal/ingest"
	"mediarun/internal/mediaerr"
)

// Role identifies a required target slot.
type Role string

const (
	RoleVideo Role = "video"
	RoleAudio Role = "audio"
)

// Rule maps one input track ID onto a target role.
type Rule struct {
	Target Role   `json:"target"` // target slot role
	Source string `json:"source"` // input track ID, e.g. "video/0"
}

// Config is the full, hashable mapping configuration for a job.
type Config struct {
	Rules []Rule `json:"rules"`
}

// Resolved is one validated, ordered target track.
type Resolved struct {
	Role   Role
	Source ingest.TrackDesc
}

// Plan is the validated mapping result consumed by remux and the trace.
type Plan struct {
	Targets []Resolved
	byID    map[string]ingest.TrackDesc
}

// requiredRoles defines what every deliverable must contain.
var requiredRoles = map[Role]bool{RoleVideo: true, RoleAudio: true}

// compatibleCodecs lists the documented codec families per role.
var compatibleCodecs = map[Role]map[string]bool{
	RoleVideo: {"avc1": true, "hvc1": true, "mp4v": true, "vcb1": true},
	RoleAudio: {"mp4a": true, "pcm1": true},
}

// Build validates the config against the parsed container tracks and returns
// an ordered plan. Validation failures are structured INVALID_CONFIG errors
// naming the offending rule; the caller must refuse to commit output.
func Build(cfg Config, tracks []ingest.TrackDesc) (*Plan, error) {
	byID := map[string]ingest.TrackDesc{}
	for _, tr := range tracks {
		byID[tr.ID] = tr
	}
	seenRole := map[Role]string{}
	var targets []Resolved
	for i, rule := range cfg.Rules {
		if rule.Target != RoleVideo && rule.Target != RoleAudio {
			return nil, mediaerr.New(mediaerr.CodeInvalidConfig, fmt.Sprintf(
				"rule %d: unknown target role %q", i, rule.Target))
		}
		if prev, dup := seenRole[rule.Target]; dup {
			return nil, mediaerr.New(mediaerr.CodeInvalidConfig, fmt.Sprintf(
				"rule %d: duplicate mapping to target %s (already fed by %s)",
				i, rule.Target, prev))
		}
		src, ok := byID[rule.Source]
		if !ok {
			return nil, mediaerr.New(mediaerr.CodeInvalidConfig, fmt.Sprintf(
				"rule %d: source track %q not found in input", i, rule.Source))
		}
		if Role(src.Kind) != rule.Target {
			return nil, mediaerr.New(mediaerr.CodeInvalidConfig, fmt.Sprintf(
				"rule %d: source %s is %s but target role is %s",
				i, rule.Source, src.Kind, rule.Target))
		}
		if !compatibleCodecs[rule.Target][src.Codec] {
			return nil, mediaerr.New(mediaerr.CodeInvalidConfig, fmt.Sprintf(
				"rule %d: codec %q on %s is incompatible with target %s",
				i, src.Codec, rule.Source, rule.Target))
		}
		seenRole[rule.Target] = rule.Source
		targets = append(targets, Resolved{Role: rule.Target, Source: src})
	}
	for role := range requiredRoles {
		if _, ok := seenRole[role]; !ok {
			return nil, mediaerr.New(mediaerr.CodeInvalidConfig, fmt.Sprintf(
				"missing required track for target role %s", role))
		}
	}
	// Deterministic target order: video first, then audio, then any others.
	sort.SliceStable(targets, func(i, j int) bool {
		return roleOrdinal(targets[i].Role) < roleOrdinal(targets[j].Role)
	})
	return &Plan{Targets: targets, byID: byID}, nil
}

func roleOrdinal(r Role) int {
	switch r {
	case RoleVideo:
		return 0
	case RoleAudio:
		return 1
	default:
		return 2
	}
}

// Track returns the descriptor for an input track ID.
func (p *Plan) Track(id string) (ingest.TrackDesc, bool) {
	tr, ok := p.byID[id]
	return tr, ok
}
