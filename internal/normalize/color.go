package normalize

import (
	"fmt"

	"aircheck/internal/fault"
	"aircheck/internal/scan"
)

// fieldFamily maps a single color metadata value to its family.
var fieldFamily = map[string]string{
	"bt709": "bt709",
	"smpte170m": "bt601", "bt470bg": "bt601",
	"bt2020nc": "bt2020", "bt2020": "bt2020", "smpte2084": "bt2020",
}

// ColorPolicy declares how color metadata deviations are handled.
type ColorPolicy struct {
	Target     string `json:"target"`      // e.g. "bt709"
	OnConflict string `json:"on_conflict"` // "reject" or "tag" (degrade: tag as target)
}

// DefaultColorPolicy rejects conflicting metadata.
func DefaultColorPolicy() ColorPolicy {
	return ColorPolicy{Target: "bt709", OnConflict: "reject"}
}

// ColorDecision records the outcome for one video track.
type ColorDecision struct {
	Family   string   `json:"family"` // resolved family, "" if unspecified
	Retag    bool     `json:"retag"`  // output must be tagged to target
	Notes    []string `json:"notes,omitempty"`
}

// CheckColor resolves a video track's color metadata. Unspecified fields
// are tagged to the target; contradictory fields fault with
// CodeColorConflict unless policy degrades to retagging.
func CheckColor(t scan.Track, p ColorPolicy) (ColorDecision, error) {
	var d ColorDecision
	if t.Kind != "video" {
		return d, nil
	}
	cs, cp, ct := norm(t.ColorSpace), norm(t.ColorPrim), norm(t.ColorTransfer)
	if cs == "" && cp == "" && ct == "" {
		d.Retag = true
		d.Notes = append(d.Notes, "unspecified color metadata, tagging "+p.Target)
		return d, nil
	}
	// Every specified field must resolve to the same family; mixed
	// families are a conflict, unknown values are a conflict too.
	fams := map[string]bool{}
	for _, v := range []string{cs, cp, ct} {
		if v == "" {
			continue
		}
		f, ok := fieldFamily[v]
		if !ok {
			fams["unknown:" + v] = true
			continue
		}
		fams[f] = true
	}
	if len(fams) > 1 {
		err := fault.New(fault.CodeColorConflict, t.ID, -1, p.OnConflict == "tag",
			"conflicting color metadata space=%s primaries=%s transfer=%s", cs, cp, ct)
		if p.OnConflict != "tag" {
			return d, err
		}
		d.Notes = append(d.Notes, err.Error())
		d.Retag = true
		return d, nil
	}
	for f := range fams {
		d.Family = f
	}
	if cs == "" || cp == "" || ct == "" {
		d.Retag = true
		d.Notes = append(d.Notes, "partial color metadata, completing tag "+p.Target)
	}
	if d.Family != p.Target {
		d.Retag = true
		d.Notes = append(d.Notes, fmt.Sprintf("color family %s != target %s, retagging", d.Family, p.Target))
	}
	return d, nil
}

func norm(s string) string {
	if s == "unknown" || s == "unspecified" {
		return ""
	}
	return s
}

// ColorArgs renders ffmpeg flags tagging output to the target family.
func ColorArgs(target string) []string {
	switch target {
	case "bt709":
		return []string{"-colorspace", "bt709", "-color_primaries", "bt709", "-color_trc", "bt709"}
	case "bt601":
		return []string{"-colorspace", "smpte170m", "-color_primaries", "smpte170m", "-color_trc", "smpte170m"}
	default:
		return nil
	}
}
