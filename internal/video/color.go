// Package video normalizes and audits color metadata conflicts.
package video

import (
	"fmt"
	"strings"

	"mediad/internal/media"
)

// Report records color normalization.
type Report struct {
	TrackID uint32         `json:"track_id"`
	OK      bool           `json:"ok"`
	Repairs int            `json:"repairs"`
	Faults  []*media.Fault `json:"faults,omitempty"`
}

// Validate unifies common color descriptors. A resolution-derived standard is
// explicit repair evidence; an HDR/bit-depth contradiction without such evidence
// marks the track failed instead of guessing.
func Validate(t *media.Track) Report {
	rep := Report{TrackID: trackID(t), OK: true}
	if t == nil || t.Kind != media.KindVideo {
		return rep
	}
	if t.Color == nil {
		t.Color = &media.ColorMetadata{BitDepth: 8, Primaries: "unknown", Transfer: "unknown", Matrix: "unknown"}
	}
	c := t.Color
	original := fmt.Sprintf("primaries=%s transfer=%s matrix=%s range=%v depth=%d detail=%q",
		c.Primaries, c.Transfer, c.Matrix, c.FullRange, c.BitDepth, c.ConflictDetail)

	expectedFamily := familyForResolution(t.Width, t.Height)
	conflict := detectConflict(c)
	if conflict != "" {
		f := media.NewFault(media.CodeColorConflict, "%s", conflict).
			WithTrack(t.ID).WithBox("colr/vcol").WithOriginal(original)
		if expectedFamily != "" && repairableFamilyConflict(c) {
			applyFamily(c, expectedFamily)
			f.WithBasis(fmt.Sprintf("resolution %dx%d implies %s", t.Width, t.Height, expectedFamily))
			c.RepairBasis = f.RepairBasis
			rep.Repairs++
		} else {
			f.Fatal().WithBasis("no resolution or descriptor evidence resolves the conflict")
			rep.OK = false
			t.Failed = true
			t.FailureReason = "ambiguous color metadata conflict"
		}
		rep.Faults = append(rep.Faults, f)
		t.Faults = append(t.Faults, f)
	}

	if c.BitDepth == 0 {
		c.BitDepth = 8
	}
	return rep
}

func detectConflict(c *media.ColorMetadata) string {
	p := canonicalFamily(c.Primaries)
	m := canonicalFamily(c.Matrix)
	if p != "" && m != "" && p != "unknown" && m != "unknown" && p != m {
		return fmt.Sprintf("primaries family %q conflicts with matrix family %q", c.Primaries, c.Matrix)
	}
	if (strings.EqualFold(c.Transfer, "pq") || strings.EqualFold(c.Transfer, "hlg")) && c.BitDepth != 0 && c.BitDepth < 10 {
		return fmt.Sprintf("HDR transfer %s requires at least 10 bits but bit_depth=%d", c.Transfer, c.BitDepth)
	}
	return ""
}

func repairableFamilyConflict(c *media.ColorMetadata) bool {
	// Family mismatches can be resolved by resolution. HDR depth contradictions
	// cannot: either transfer or bit depth may have been corrupted.
	p := canonicalFamily(c.Primaries)
	m := canonicalFamily(c.Matrix)
	return p != "" && m != "" && p != m
}

func familyForResolution(w, h uint16) string {
	switch {
	case h >= 2160 || w >= 3840:
		return "bt2020"
	case h >= 720 || w >= 1280:
		return "bt709"
	case h > 0:
		return "bt601"
	default:
		return ""
	}
}

func canonicalFamily(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	switch {
	case strings.Contains(v, "2020"):
		return "bt2020"
	case strings.Contains(v, "709"):
		return "bt709"
	case strings.Contains(v, "601"):
		return "bt601"
	case v == "":
		return "unknown"
	default:
		return "unknown"
	}
}

func applyFamily(c *media.ColorMetadata, family string) {
	switch family {
	case "bt2020":
		c.Primaries, c.Matrix = "bt2020", "bt2020nc"
		if c.Transfer == "" || canonicalFamily(c.Transfer) != family {
			c.Transfer = "pq"
			c.BitDepth = maxU8(c.BitDepth, 10)
		}
	case "bt709":
		c.Primaries, c.Transfer, c.Matrix = "bt709", "bt709", "bt709"
	case "bt601":
		c.Primaries, c.Transfer, c.Matrix = "bt601", "bt601", "bt601"
	}
}

func maxU8(a, b uint8) uint8 {
	if a > b {
		return a
	}
	return b
}

func trackID(t *media.Track) uint32 {
	if t == nil {
		return 0
	}
	return t.ID
}
