package output

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"relay/internal/fixture"
	"relay/internal/ingest"
	"relay/internal/timeline"
)

func hashHex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func TestSerializeRoundTrip(t *testing.T) {
	spec := fixture.FragmentSpec{
		TrackID: 101, Sequence: 8, Tfdt: 12000,
		Samples: []fixture.SampleSpec{
			{Duration: 3000, Payload: fixture.SamplePayload(101, 8, 1, 64), Sync: true},
			{Duration: 3000, Payload: fixture.SamplePayload(101, 8, 2, 48)},
		},
	}
	raw := fixture.BuildFragment(spec)
	f, err := ingest.ParseFragment("in", 0, raw)
	if err != nil {
		t.Fatal(err)
	}
	rf := &timeline.RepairedFragment{
		Fragment: f, NewSequence: 7, GlobalDTS: 18000,
		RepairedTfdt: 42000, RawHash: hashHex(raw),
	}
	out, err := Serialize(rf)
	if err != nil {
		t.Fatal(err)
	}
	re, err := ingest.ParseFragment("out", 0, out)
	if err != nil {
		t.Fatalf("reparse failed: %v", err)
	}
	if re.Sequence != 7 {
		t.Fatalf("mfhd not rewritten: %d", re.Sequence)
	}
	if re.Tfdt != 42000 {
		t.Fatalf("tfdt not rewritten: %d", re.Tfdt)
	}
	if len(re.Samples) != 2 || !re.Samples[0].Sync || re.Samples[1].Sync {
		t.Fatalf("sample flags changed: %+v", re.Samples)
	}
	if string(re.Mdat) != string(f.Mdat) {
		t.Fatal("mdat payload must be byte-stable")
	}
}

func TestDeterministicBytes(t *testing.T) {
	spec := fixture.FragmentSpec{
		TrackID: 201, Sequence: 1, Tfdt: 0,
		Samples: []fixture.SampleSpec{
			{Duration: 1024, Payload: fixture.SamplePayload(201, 1, 1, 32), Sync: true},
		},
	}
	raw := fixture.BuildFragment(spec)
	f, _ := ingest.ParseFragment("in", 0, raw)
	rf := &timeline.RepairedFragment{Fragment: f, NewSequence: 1, RepairedTfdt: 0}
	a, err := Serialize(rf)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Serialize(rf)
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) {
		t.Fatal("serialization must be deterministic")
	}
}
