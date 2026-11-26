package trackmap

import (
	"errors"
	"testing"

	"mediad/internal/bmff"
	"mediad/internal/model"
)

func testIndex() *bmff.Index {
	return &bmff.Index{Tracks: []bmff.Track{
		{
			ID: 1, Handler: "vide", Codec: "avc1", Timescale: 30000,
			Samples: []bmff.Sample{
				{Index: 0, TrackID: 1, DTS: 0, PTS: 0, Duration: 1001},
				{Index: 1, TrackID: 1, DTS: 1001, PTS: 3003, Duration: 1001},
				{Index: 2, TrackID: 1, DTS: 2002, PTS: 2002, Duration: 1001},
			},
		},
		{ID: 2, Handler: "soun", Codec: "mp4a", Timescale: 48000, Samples: []bmff.Sample{{Index: 0, TrackID: 2, DTS: 0, PTS: 0, Duration: 1024}}},
		{ID: 3, Codec: "encv", Encrypted: true, Samples: []bmff.Sample{{Index: 0, DTS: 0}}},
	}}
}

func TestExplicitMappingConvertsRationalTime(t *testing.T) {
	plan, err := BuildPlan(testIndex(), []Request{{SourceID: 1, OutputID: 10, OutputTimescale: 60000}})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Tracks) != 1 || plan.Tracks[0].OutputID != 10 {
		t.Fatalf("bad plan: %+v", plan.Tracks)
	}
	s := plan.Tracks[0].Samples
	if s[1].DTS != 2002 || s[1].PTS != 6006 {
		t.Fatalf("exact conversion failed: dts=%d pts=%d", s[1].DTS, s[1].PTS)
	}
	if s[0].DurationOut != 2002 {
		t.Fatalf("duration_out=%d want 2002", s[0].DurationOut)
	}
	if plan.Digest == "" || len(plan.Digest) != 64 {
		t.Fatalf("digest=%q", plan.Digest)
	}
}

func TestAmbiguousMappingsFail(t *testing.T) {
	idx := testIndex()
	cases := [][]Request{
		nil,
		{{SourceID: 1, OutputID: 10}, {SourceID: 1, OutputID: 11}},
		{{SourceID: 1, OutputID: 10}, {SourceID: 2, OutputID: 10}},
		{{SourceID: 99, OutputID: 1}},
	}
	for i, req := range cases {
		_, err := BuildPlan(idx, req)
		var me *model.Error
		if !errors.As(err, &me) || me.Code != model.CodeAmbiguousMap {
			t.Fatalf("case %d: expected ambiguous_map, got %v", i, err)
		}
	}
}

func TestEncryptedTrackFails(t *testing.T) {
	_, err := BuildPlan(testIndex(), []Request{{SourceID: 3, OutputID: 30}})
	var me *model.Error
	if !errors.As(err, &me) || me.Code != model.CodeEncryptedTrack {
		t.Fatalf("expected encrypted_track, got %v", err)
	}
}

func TestTimestampRegressionFails(t *testing.T) {
	idx := testIndex()
	idx.Tracks[0].Samples[2].DTS = 500
	_, err := BuildPlan(idx, []Request{{SourceID: 1, OutputID: 10}})
	var me *model.Error
	if !errors.As(err, &me) || me.Code != model.CodeTimestampBackward {
		t.Fatalf("expected timestamp_backward, got %v", err)
	}
}

func TestDigestStable(t *testing.T) {
	req := []Request{{SourceID: 2, OutputID: 20}, {SourceID: 1, OutputID: 10}}
	a, err := Digest(req)
	if err != nil {
		t.Fatal(err)
	}
	reversed := []Request{req[1], req[0]}
	b, err := Digest(reversed)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatal("digest should be order-independent after canonicalization")
	}
}
