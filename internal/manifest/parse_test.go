package manifest

import (
	"strings"
	"testing"
)

func TestParseOK(t *testing.T) {
	in := `
# comment
{"track_id":"v1","path":"a.mp4","kind":"video","timescale":90000,"edits":[{"media_start":90000,"duration":180000}]}
{"track_id":"a1","path":"a.mp4","kind":"audio","timescale":48000,"depends_on":["v1"]}
`
	m, err := Parse(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Tracks) != 2 || m.Tracks[0].ID != "v1" || m.Tracks[1].Line != 4 {
		t.Fatalf("tracks: %+v", m.Tracks)
	}
	if m.Find("a1") == nil || m.Find("zz") != nil {
		t.Fatal("find")
	}
}

func TestParseErrorsReportLineAndTrack(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"bad json", `{"track_id":`, "line 1"},
		{"dup id", "{\"track_id\":\"v\",\"path\":\"a.mp4\",\"kind\":\"video\",\"timescale\":90000}\n{\"track_id\":\"v\",\"path\":\"b.mp4\",\"kind\":\"video\",\"timescale\":90000}", "line 2 (track \"v\")"},
		{"bad kind", `{"track_id":"v","path":"a.mp4","kind":"data","timescale":90000}`, "line 1 (track \"v\")"},
		{"bad edit", `{"track_id":"v","path":"a.mp4","kind":"video","timescale":90000,"edits":[{"media_start":0,"duration":-5}]}`, "non-positive duration"},
		{"escape path", `{"track_id":"v","path":"../x.mp4","kind":"video","timescale":90000}`, "work directory"},
		{"empty", "", "no tracks"},
	}
	for _, c := range cases {
		_, err := Parse(strings.NewReader(c.in))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want substring %q", c.name, err, c.want)
		}
		var pe *ParseError
		if err != nil && !asParseError(err, &pe) {
			t.Errorf("%s: not a ParseError", c.name)
		}
	}
}

func asParseError(err error, out **ParseError) bool {
	for err != nil {
		if pe, ok := err.(*ParseError); ok {
			*out = pe
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
