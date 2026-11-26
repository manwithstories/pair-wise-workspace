package color

import "testing"

func TestApplySRGB(t *testing.T) {
	cases := []struct {
		tag, out string
		action   Action
	}{
		{"srgb", "srgb", Keep},
		{"bt709", "srgb", Normalized},
		{"bt2020", "srgb", Normalized},
		{"display-p3", "srgb", Normalized},
		{"acescg", "acescg", UnknownPreserved},
		{"", "", UnknownPreserved},
	}
	for _, c := range cases {
		out, a := ApplySRGB(c.tag)
		if out != c.out || a != c.action {
			t.Fatalf("ApplySRGB(%q) = (%q,%q), want (%q,%q)", c.tag, out, a, c.out, c.action)
		}
	}
}
