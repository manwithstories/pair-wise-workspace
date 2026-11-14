package mpd

import "testing"

func TestExpandTemplate(t *testing.T) {
	v := TemplateVars{RepresentationID: "r1", Bandwidth: 400000, Number: 7, Time: 1665000}
	cases := map[string]string{
		"$Number%05d$.m4s":                "00007.m4s",
		"$Time%09d$.m4s":                  "001665000.m4s",
		"$RepresentationID$_$Number$.m4s": "r1_7.m4s",
		"bw_$Bandwidth$.m4s":              "bw_400000.m4s",
		"100$$_seg.m4s":                   "100$_seg.m4s",
	}
	for tpl, want := range cases {
		got, err := ExpandTemplate(tpl, v)
		if err != nil || got != want {
			t.Errorf("ExpandTemplate(%q) = %q, %v; want %q", tpl, got, err, want)
		}
	}
}

func TestExpandTemplateErrors(t *testing.T) {
	v := TemplateVars{}
	for _, tpl := range []string{"$Number", "$Foo$.m4s", "$Number%xd$.m4s", "$RepresentationID%05d$"} {
		if _, err := ExpandTemplate(tpl, v); err == nil {
			t.Errorf("want error for %q", tpl)
		}
	}
}

func TestAddressingMode(t *testing.T) {
	if got := AddressingMode("$Number%05d$.m4s"); got != AddrNumber {
		t.Fatalf("got %q", got)
	}
	if got := AddressingMode("$Time$.m4s"); got != AddrTime {
		t.Fatalf("got %q", got)
	}
	if got := AddressingMode("plain.m4s"); got != "" {
		t.Fatalf("got %q", got)
	}
}
