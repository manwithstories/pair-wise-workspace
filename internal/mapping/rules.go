// Package mapping selects input tracks, assigns target track numbers and
// validates the mapping before any output bytes are written.
package mapping

import (
	"fmt"
	"sort"
	"strings"

	"mediamerge/internal/clock"
	"mediamerge/internal/demux"
	"mediamerge/internal/model"
)

// Rule is one explicit input-to-target track mapping. An empty Input selects
// by position in the sorted input track list.
type Rule struct {
	Input    string // input track id, e.g. "1" or "audio:1"; empty means positional
	Target   uint32
	Media    model.MediaType
	Required bool
}

// Plan is the validated mapping result.
type Plan struct {
	Tracks []*demux.Track
	Rules  []Rule
	Map    map[uint32]*demux.Track
}

// ParseRules parses compact CLI syntax: "1=10:video,2=11:audio".
func ParseRules(spec string) ([]Rule, error) {
	if strings.TrimSpace(spec) == "" {
		return nil, nil
	}
	var out []Rule
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		var r Rule
		left, right, ok := strings.Cut(part, "=")
		if !ok {
			return nil, fmt.Errorf("mapping: rule %q is missing '='", part)
		}
		r.Input = strings.TrimSpace(left)
		fields := strings.Split(right, ":")
		var target uint64
		if _, err := fmt.Sscanf(strings.TrimSpace(fields[0]), "%d", &target); err != nil || target == 0 {
			return nil, fmt.Errorf("mapping: rule %q has invalid target %q", part, fields[0])
		}
		r.Target = uint32(target)
		if len(fields) > 1 {
			switch strings.TrimSpace(fields[1]) {
			case "video":
				r.Media = model.MediaVideo
			case "audio":
				r.Media = model.MediaAudio
			default:
				return nil, fmt.Errorf("mapping: rule %q has unknown media %q", part, fields[1])
			}
		}
		out = append(out, r)
	}
	return out, nil
}

// DefaultRules assigns target ids 1..n in input order, preserving media type.
func DefaultRules(tracks []*demux.Track) []Rule {
	rules := make([]Rule, len(tracks))
	for i, t := range tracks {
		rules[i] = Rule{Input: fmt.Sprint(t.InputID), Target: uint32(i + 1), Media: t.MediaType, Required: true}
	}
	return rules
}

// Apply builds and validates a Plan. Duplicate target ids, missing required
// tracks, media mismatches and unsupported timescales all fail here.
func Apply(tracks []*demux.Track, rules []Rule) (*Plan, error) {
	if len(tracks) == 0 {
		return nil, mappingErr(model.CodeMapping, 0, "input has no tracks")
	}
	if len(rules) == 0 {
		rules = DefaultRules(tracks)
	}
	byInput := map[string]*demux.Track{}
	sorted := append([]*demux.Track(nil), tracks...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].InputID < sorted[j].InputID })
	for _, t := range sorted {
		byInput[fmt.Sprint(t.InputID)] = t
		byInput[string(t.MediaType)+":"+fmt.Sprint(t.InputID)] = t
	}
	plan := &Plan{Rules: rules, Map: map[uint32]*demux.Track{}}
	usedInput := map[uint32]bool{}
	usedTarget := map[uint32]bool{}
	for pos, r := range rules {
		var t *demux.Track
		switch {
		case r.Input != "":
			t = byInput[r.Input]
			if t == nil {
				if r.Required {
					return nil, mappingErr(model.CodeMissingBox, r.Target,
						"required input track %q not found", r.Input)
				}
				continue
			}
		case pos < len(sorted):
			t = sorted[pos]
		default:
			if r.Required {
				return nil, mappingErr(model.CodeMissingBox, r.Target,
					"required positional track %d not found", pos)
			}
			continue
		}
		if r.Target == 0 {
			return nil, mappingErr(model.CodeMapping, t.InputID, "target track id 0 is reserved")
		}
		if usedTarget[r.Target] {
			return nil, mappingErr(model.CodeMapping, t.InputID,
				"duplicate target track id %d", r.Target)
		}
		if usedInput[t.InputID] {
			return nil, mappingErr(model.CodeMapping, t.InputID,
				"input track %d mapped more than once", t.InputID)
		}
		media := r.Media
		if media == "" {
			media = t.MediaType
		}
		if media != t.MediaType {
			return nil, mappingErr(model.CodeMapping, t.InputID,
				"input track %d is %s, rule declares %s", t.InputID, t.MediaType, media)
		}
		if t.Timescale == 0 {
			return nil, mappingErr(model.CodeTimebase, t.InputID,
				"track %d has zero timescale", t.InputID)
		}
		if _, err := clock.FromTimescale(t.Timescale); err != nil {
			return nil, mappingErr(model.CodeTimebase, t.InputID, "track %d: %v", t.InputID, err)
		}
		t.TargetID = r.Target
		usedInput[t.InputID] = true
		usedTarget[r.Target] = true
		plan.Tracks = append(plan.Tracks, t)
		plan.Map[r.Target] = t
	}
	if len(plan.Tracks) == 0 {
		return nil, mappingErr(model.CodeMapping, 0, "mapping selected no tracks")
	}
	return plan, nil
}

// OutputTimescale picks the movie timescale: the largest selected track
// timescale, which keeps every track's timestamps integral while preserving
// exact rational conversion.
func (p *Plan) OutputTimescale() uint32 {
	var max uint32 = 1
	for _, t := range p.Tracks {
		if t.Timescale > max {
			max = t.Timescale
		}
	}
	return max
}

// TotalSamples returns the number of samples to be written.
func (p *Plan) TotalSamples() int {
	n := 0
	for _, t := range p.Tracks {
		n += len(t.Samples)
	}
	return n
}

func mappingErr(code string, track uint32, format string, args ...interface{}) *model.Error {
	return model.NewError(model.StageMapping, code, -1, track, false, format, args...)
}
