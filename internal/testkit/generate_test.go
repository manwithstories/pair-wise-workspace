package testkit

import (
	"os"
	"testing"
)

func TestGenerateStandardLayout(t *testing.T) {
	init, frags := StandardSampleLayout()
	m, err := Generate(t.TempDir(), init, frags)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Segments) != 13 {
		t.Fatalf("segments = %d, want 13", len(m.Segments))
	}
	if len(m.Digest) != 64 {
		t.Fatalf("digest = %q", m.Digest)
	}
	if m.Segments[0].Name != "init.mp4" || !m.Segments[0].Init {
		t.Fatalf("first segment = %+v", m.Segments[0])
	}
	for _, s := range m.Segments {
		info, err := os.Stat(s.Path)
		if err != nil || info.Size() != int64(s.Bytes) {
			t.Fatalf("segment %s stat mismatch: %v", s.Name, err)
		}
		if len(s.SHA256) != 64 {
			t.Fatalf("segment %s sha = %q", s.Name, s.SHA256)
		}
	}
}

func TestGenerateDeterministic(t *testing.T) {
	init, frags := StandardSampleLayout()
	a, err := Generate(t.TempDir(), init, frags)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Generate(t.TempDir(), init, frags)
	if err != nil {
		t.Fatal(err)
	}
	if a.Digest != b.Digest {
		t.Fatalf("digests differ: %s != %s", a.Digest, b.Digest)
	}
	for i := range a.Segments {
		if a.Segments[i].SHA256 != b.Segments[i].SHA256 {
			t.Fatalf("segment %d differs", i)
		}
	}
}
