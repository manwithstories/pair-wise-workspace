package mux_test

import (
	"bytes"
	"testing"

	"github.com/abema/go-mp4"

	"mediacheck/internal/mux"
)

func TestBuildFMP4IsDeterministicAndParseable(t *testing.T) {
	video := []mux.Fragment{
		{TrackID: 1, BasePTS: 0, Samples: []mux.Sample{{PTS: 0, Duration: 3000, Data: make([]byte, 120), Keyframe: true}}},
		{TrackID: 1, BasePTS: 3000, Samples: []mux.Sample{{PTS: 3000, Duration: 3000, Data: make([]byte, 140)}}},
	}
	audio := mux.Fragment{TrackID: 2, BasePTS: 0, Samples: []mux.Sample{{Duration: 1920, Data: make([]byte, 200)}}}
	cfg := mux.Config{DurationTicks: 6000, Width: 16, Height: 16, AudioChannels: 2}

	first, err := mux.BuildFMP4(cfg, video, &audio)
	if err != nil {
		t.Fatal(err)
	}
	second, err := mux.BuildFMP4(cfg, video, &audio)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.Bytes, second.Bytes) {
		t.Fatal("fMP4 output is not deterministic")
	}
	if len(first.Segments) != 2 {
		t.Fatalf("segments=%d", len(first.Segments))
	}
	if !bytes.Contains(first.Bytes, []byte("ftyp")) || !bytes.Contains(first.Bytes, []byte("moov")) {
		t.Fatal("missing init boxes")
	}

	r := bytes.NewReader(first.Bytes)
	truns, err := mp4.ExtractBox(r, nil, mp4.BoxPath{
		mp4.BoxTypeMoof(), mp4.BoxTypeTraf(), mp4.BoxTypeTrun(),
	})
	if err != nil {
		t.Fatalf("extract trun: %v", err)
	}
	if len(truns) != 3 {
		t.Fatalf("trun boxes=%d want 3", len(truns))
	}
}

func TestAnnexBLengthConversion(t *testing.T) {
	nal := []byte{0x65, 0x01, 0x02}
	out := mux.AnnexBToLengthPrefixed(nal)
	if got := uint32(out[0])<<24 | uint32(out[1])<<16 | uint32(out[2])<<8 | uint32(out[3]); got != 3 {
		t.Fatalf("length=%d", got)
	}
	if !bytes.Equal(out[4:], nal) {
		t.Fatal("NAL payload changed")
	}
}
