package index

import (
	"bytes"
	"os"
	"testing"
)

func TestSmokeRoundTrip(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "idx-*.mp4")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	b := NewBuilder(f)
	if err := b.Begin(make([]byte, 16)); err != nil {
		t.Fatal(err)
	}
	pcm1 := []byte{1, 2, 3, 4}
	pcm2 := []byte{5, 6, 7, 8, 9, 10}
	off1, len1, err := b.AddPCM(pcm1)
	if err != nil {
		t.Fatal(err)
	}
	off2, len2, err := b.AddPCM(pcm2)
	if err != nil {
		t.Fatal(err)
	}
	descs := []Descriptor{{
		TrackID: 1, DeviceID: "d1", SampleRate: 48000, Channels: 2, BitsPerSample: 16,
		Entries: []Entry{
			{TrackID: 1, StartSample: 0, SampleCount: 2, DataOffset: off1, DataLength: len1, RTPSeq: 0},
			{TrackID: 1, StartSample: 2, SampleCount: 3, DataOffset: off2, DataLength: len2, RTPSeq: 1},
		},
	}}
	if _, err := b.Finalize(descs); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	raw, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	r := bytes.NewReader(raw)
	got, mdatBase, digest, err := ReadFile(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got[1]) != 2 {
		t.Fatalf("entries=%d", len(got[1]))
	}
	if got[1][1].DataOffset != off2 {
		t.Fatalf("off2 mismatch: %d want %d", got[1][1].DataOffset, off2)
	}
	back, err := ReadAt(r, mdatBase, off2, int(len2))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(back, pcm2) {
		t.Fatalf("seek readback mismatch: %v", back)
	}
	if len(digest) != 16 {
		t.Fatalf("digest len %d", len(digest))
	}
	if se := VerifyEntries(1, got[1]); se != nil {
		t.Fatalf("verify: %v", se)
	}
}
