package manifest

import (
	"strings"
	"testing"

	"archive/internal/media"
)

const good = `{
  "asset": "demo",
  "movie_timescale": 1000000,
  "program_tracks": ["v1", "a1"],
  "tracks": [
    {"id": "v1", "type": "video", "timescale": 90000,
     "samples": [{"dts": 9000, "cts_offset": 0, "duration": 3000, "size": 100, "keyframe": true}],
     "edit_list": [{"media_time": -1, "segment_duration": 100000}]},
    {"id": "a1", "type": "audio", "timescale": 48000,
     "samples": [{"dts": 4800, "cts_offset": 0, "duration": 1024, "size": 50, "keyframe": true}],
     "edit_list": []}
  ]
}`

func TestParseOK(t *testing.T) {
	m, err := Parse(strings.NewReader(good))
	if err != nil {
		t.Fatal(err)
	}
	if m.Asset != "demo" || len(m.Tracks) != 2 {
		t.Fatalf("bad manifest: %+v", m)
	}
	if m.Track("a1").Timescale != 48000 {
		t.Fatal("Track lookup broken")
	}
	if m.Track("nope") != nil {
		t.Fatal("expected nil for unknown track")
	}
}

func TestNegativeDurationReportsTrackAndOffset(t *testing.T) {
	bad := strings.Replace(good, `"duration": 3000`, `"duration": -5`, 1)
	_, err := Parse(strings.NewReader(bad))
	me, ok := err.(*media.Error)
	if !ok {
		t.Fatalf("expected *media.Error, got %T", err)
	}
	if me.Track != "v1" || me.Offset != 0 {
		t.Fatalf("want track=v1 offset=0, got %v", me)
	}
	if media.ExitCode(err) != 2 {
		t.Fatal("corrupt input must map to exit code 2")
	}
}

func TestMissingTrackReported(t *testing.T) {
	bad := strings.Replace(good, `"program_tracks": ["v1", "a1"]`,
		`"program_tracks": ["v1", "a2"]`, 1)
	_, err := Parse(strings.NewReader(bad))
	me := err.(*media.Error)
	if me.Track != "a2" || me.Offset != 1 {
		t.Fatalf("missing track error wrong: %v", me)
	}
}

func TestSyntaxErrorCarriesByteOffset(t *testing.T) {
	_, err := Parse(strings.NewReader(`{"asset": `))
	me := err.(*media.Error)
	if me.Offset < 0 {
		t.Fatalf("syntax error must carry byte offset: %v", me)
	}
}

func TestBadEditListAndTimescale(t *testing.T) {
	bad := strings.Replace(good, `"segment_duration": 100000`, `"segment_duration": -1`, 1)
	if _, err := Parse(strings.NewReader(bad)); err == nil {
		t.Fatal("negative edit duration accepted")
	}
	bad = strings.Replace(good, `"timescale": 90000`, `"timescale": 0`, 1)
	if _, err := Parse(strings.NewReader(bad)); err == nil {
		t.Fatal("zero timescale accepted")
	}
}
