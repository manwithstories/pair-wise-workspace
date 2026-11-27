package normalize

import (
	"fmt"

	"aircheck/internal/fault"
	"aircheck/internal/scan"
)

// colorFamilies groups equivalent color metadata triples.
var colorFamilies = map[string]string{
	"bt709|bt709|bt709":                 "bt709",
	"smpte170m|smpte170m|smpte170m":     "bt601",
	"bt470bg|bt470bg|bt470bg":           "bt601",
	"bt2020nc|bt2020|bt2020nc":          "bt2020",
	"bt2020nc|bt2020|smpte2084":         "bt2020-hdr",
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
	key := fmt.Sprintf("%s|%s|%s", orUnk(cs), orUnk(cp), orUnk(ct))
	fam, ok := colorFamilies[key]
	if !ok {
		err := fault.New(fault.CodeColorConflict, t.ID, -1, p.OnConflict == "tag",
			"conflicting color metadata space=%s primaries=%s transfer=%s", cs, cp, ct)
		if p.OnConflict != "tag" {
			return d, err
		}
		d.Notes = append(d.Notes, err.Error())
		d.Retag = true
		return d, nil
	}
	d.Family = fam
	if fam != p.Target {
		d.Retag = true
		d.Notes = append(d.Notes, fmt.Sprintf("color family %s != target %s, retagging", fam, p.Target))
	}
	return d, nil
}

func norm(s string) string {
	if s == "unknown" || s == "unspecified" {
		return ""
	}
	return s
}

func orUnk(s string) string {
	if s == "" {
		return "unspecified"
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
