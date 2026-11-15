package ladder

import "testing"

const spec = "240p:300k,360p:800k,480p:1200k,720p:2500k,1080p:5000k;aac:64k,128k"

func TestParse(t *testing.T) {
	specs, err := Parse(spec)
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 7 {
		t.Fatalf("specs=%d want 7", len(specs))
	}
	if specs[0].Bandwidth != 300000 || specs[4].Bandwidth != 5000000 || specs[6].Bandwidth != 128000 {
		t.Fatalf("bandwidths: %v %v %v", specs[0].Bandwidth, specs[4].Bandwidth, specs[6].Bandwidth)
	}
	v, a := Group(specs)
	if len(v) != 5 || len(a) != 2 {
		t.Fatalf("group: %d %d", len(v), len(a))
	}
	if probs := Validate(specs); len(probs) != 0 {
		t.Fatalf("validate: %v", probs)
	}
}

func TestValidateRejects(t *testing.T) {
	bad := []Spec{
		{Name: "x", Kind: "video", Bandwidth: 1, Width: 853, Height: 480, FrameRate: "30000/1001", Codecs: "avc1.4D401F"},
		{Name: "y", Kind: "video", Bandwidth: 1, Width: 640, Height: 360, FrameRate: "abc", Codecs: "avc1.4D401F"},
		{Name: "z", Kind: "audio", Bandwidth: 1, Codecs: "opus"},
		{Name: "w", Kind: "video", Bandwidth: -1, Width: 640, Height: 360, FrameRate: "25", Codecs: "avc1.4D401F"},
	}
	probs := Validate(bad)
	codes := map[string]int{}
	for _, p := range probs {
		codes[p.Code]++
	}
	if codes["E_BAD_GEOMETRY"] != 1 || codes["E_BAD_FRAMERATE"] != 1 ||
		codes["E_BAD_CODEC"] != 1 || codes["E_BAD_BANDWIDTH"] != 1 {
		t.Fatalf("codes: %v", codes)
	}
}

func TestParseRejects(t *testing.T) {
	for _, s := range []string{"", "240p", "240p:300k", "999p:300k;aac:64k", "240p:3x;aac:64k"} {
		if _, err := Parse(s); err == nil {
			t.Fatalf("accepted %q", s)
		}
	}
}
