// Package color checks color metadata tags against the manifest policy.
package color

// Action describes what the policy did with a color tag.
type Action string

const (
	// Keep: the tag already matches the policy.
	Keep Action = "keep"
	// Normalized: a known conflicting tag was normalized to the policy.
	Normalized Action = "normalized"
	// UnknownPreserved: an unknown tag was kept as-is and reported;
	// it is never claimed to be converted.
	UnknownPreserved Action = "unknown_preserved"
)

// known lists tags that the sRGB policy can normalize.
var known = map[string]bool{
	"srgb":       true,
	"bt709":      true,
	"bt2020":     true,
	"display-p3": true,
}

// ApplySRGB applies the manifest sRGB policy to a track color tag.
// Unknown tags are preserved, not converted.
func ApplySRGB(tag string) (out string, action Action) {
	switch {
	case tag == "srgb":
		return "srgb", Keep
	case known[tag]:
		return "srgb", Normalized
	default:
		return tag, UnknownPreserved
	}
}
