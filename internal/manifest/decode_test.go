package manifest

import (
	"strings"
	"testing"
)

var valid = `{
  "version": 1,
  "timebase": {"num": 1, "den": 90000},
  "color_policy": "srgb",
  "tracks": [
    {"id": "v1", "kind": "video", "codec": "png",
     "fps": {"num": 24, "den": 1}, "color_tag": "srgb",
     "frames": [
       {"seq": 0, "pts": 0, "keyframe": true, "file": "v1/0.png",
        "sha256": "` + strings.Repeat("a", 64) + `", "bytes": 10},
       {"seq": 1, "pts": 3750, "keyframe": false, "file": "v1/1.png",
        "sha256": "` + strings.Repeat("b", 64) + `", "bytes": 11}
     ]},
    {"id": "a1", "kind": "audio", "layout": "stereo", "sample_rate": 48000,
     "format": "s16le", "pts": 0, "file": "a1/x.pcm",
     "sha256": "` + strings.Repeat("c", 64) + `", "bytes": 100}
  ]
}`

func TestDecodeValid(t *testing.T) {
	m, err := Decode(strings.NewReader(valid))
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Tracks) != 2 || m.Tracks[0].Frames[1].PTS != 3750 {
		t.Fatalf("bad manifest: %+v", m)
	}
}

func TestRejectUnknownField(t *testing.T) {
	bad := strings.Replace(valid, `"color_policy"`, `"colour_policy"`, 1)
	if _, err := Decode(strings.NewReader(bad)); err == nil {
		t.Fatal("expected unknown-field rejection")
	}
}

func TestRejectDuplicateSeq(t *testing.T) {
	bad := strings.Replace(valid, `"seq": 1, "pts": 3750`, `"seq": 0, "pts": 3750`, 1)
	_, err := Decode(strings.NewReader(bad))
	if err == nil || !strings.Contains(err.Error(), "duplicate frame seq 0") {
		t.Fatalf("expected duplicate seq error, got %v", err)
	}
}

func TestRejectBadInputs(t *testing.T) {
	cases := []struct{ name, old, new string }{
		{"version", `"version": 1`, `"version": 2`},
		{"policy", `"color_policy": "srgb"`, `"color_policy": "bt709"`},
		{"codec", `"codec": "png"`, `"codec": "webp"`},
		{"format", `"format": "s16le"`, `"format": "f32le"`},
		{"rate", `"sample_rate": 48000`, `"sample_rate": -1`},
		{"sha", strings.Repeat("a", 64), "abc"},
	}
	for _, c := range cases {
		bad := strings.Replace(valid, c.old, c.new, 1)
		if _, err := Decode(strings.NewReader(bad)); err == nil {
			t.Fatalf("%s: expected error", c.name)
		}
	}
}

func TestRejectDuplicateTrackID(t *testing.T) {
	bad := strings.Replace(valid, `"id": "a1"`, `"id": "v1"`, 1)
	if _, err := Decode(strings.NewReader(bad)); err == nil {
		t.Fatal("expected duplicate track id error")
	}
}
