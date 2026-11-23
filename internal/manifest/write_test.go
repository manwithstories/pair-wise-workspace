package manifest

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestWriteDeterministic(t *testing.T) {
	m := Manifest{
		StartNs: 0, EndNs: 2_000_000_000, DurationNs: 2_000_000_000,
		SourceSHA256: "abc",
		Tracks: []Track{
			{Number: 2, Type: "audio", Codec: "A_OPUS", BaseNs: 0, BitrateKbps: 80.5},
			{Number: 1, Type: "video", Codec: "V_VP8", BaseNs: 0, BitrateKbps: 339.5},
		},
	}
	var b1, b2 bytes.Buffer
	if err := Write(&b1, m); err != nil {
		t.Fatal(err)
	}
	if err := Write(&b2, m); err != nil {
		t.Fatal(err)
	}
	if b1.String() != b2.String() {
		t.Fatal("output not deterministic")
	}
	var got Manifest
	if err := json.Unmarshal(b1.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Tracks[0].Number != 1 || got.Tracks[1].Number != 2 {
		t.Fatalf("tracks not sorted: %+v", got.Tracks)
	}
	if got.DurationNs != 2_000_000_000 || got.SourceSHA256 != "abc" {
		t.Fatalf("manifest: %+v", got)
	}
}
