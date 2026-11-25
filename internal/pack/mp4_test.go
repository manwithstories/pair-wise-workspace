package pack

import (
	"testing"
)

func TestBuildAndValidateInitAndMedia(t *testing.T) {
	init, err := BuildInit()
	if err != nil {
		t.Fatalf("BuildInit: %v", err)
	}
	if len(init) < 32 {
		t.Fatalf("init too short: %d", len(init))
	}
	media, err := BuildMedia(Fragment{
		SequenceNumber: 1,
		StartNS:        123_456,
		Samples: []Sample{
			{Data: []byte{0, 0, 0, 1, 0x65, 1}, DurationNS: 33_333_333, Keyframe: true},
			{Data: []byte{0, 0, 0, 1, 0x41, 2}, DurationNS: 33_333_333},
		},
	})
	if err != nil {
		t.Fatalf("BuildMedia: %v", err)
	}
	if len(media) < 16 {
		t.Fatalf("media too short: %d", len(media))
	}
	if err := Validate(media, "moof", "trun", "mdat"); err != nil {
		t.Fatalf("media revalidation: %v", err)
	}
}

func TestBuildMediaRejectsEmpty(t *testing.T) {
	if _, err := BuildMedia(Fragment{}); err == nil {
		t.Fatal("expected empty fragment error")
	}
}
