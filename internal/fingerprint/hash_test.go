package fingerprint

import (
	"bytes"
	"testing"

	"mediad/internal/model"
)

func src(role model.TrackRole, idx int) Source {
	return Source{Track: role, Index: idx, URI: "f.m4s"}
}

func TestFingerprint_DeterministicAndBlockAligned(t *testing.T) {
	data := bytes.Repeat([]byte("ABCDEFGHIJ"), BlockSize) // exactly N blocks
	a, err := FingerprintReader(bytes.NewReader(data), src(model.TrackStudio, 0), false)
	if err != nil {
		t.Fatal(err)
	}
	b, err := FingerprintReader(bytes.NewReader(data), src(model.TrackField, 1), false)
	if err != nil {
		t.Fatal(err)
	}
	if a.DigestHex != b.DigestHex {
		t.Fatal("identical content must hash identically across tracks")
	}
	if len(a.Blocks) != 10 || len(a.BlockHex) != 10 {
		t.Fatalf("want 10 blocks, got %d", len(a.Blocks))
	}
	if a.Bytes != int64(len(data)) {
		t.Fatalf("byte count = %d", a.Bytes)
	}
}

func TestFingerprint_PrefixIsNotFullMatch(t *testing.T) {
	full := bytes.Repeat([]byte("Z"), BlockSize*3)
	prefix := full[:BlockSize*2]
	fpFull, _ := FingerprintReader(bytes.NewReader(full), src(model.TrackStudio, 0), false)
	fpPrefix, _ := FingerprintReader(bytes.NewReader(prefix), src(model.TrackField, 0), false)
	if fpFull.DigestHex == fpPrefix.DigestHex {
		t.Fatal("different lengths must produce different full digests")
	}
}

func TestFindDuplicates_CrossTrackKeepsAllSources(t *testing.T) {
	data := []byte("shared content across tracks")
	fps := []*Fingerprint{
		mustFP(t, data, src(model.TrackStudio, 2), false),
		mustFP(t, data, src(model.TrackField, 5), false),
		mustFP(t, []byte("unique voice line"), src(model.TrackVoice, 0), false),
	}
	groups := FindDuplicates(fps)
	if len(groups) != 1 {
		t.Fatalf("want 1 duplicate group, got %+v", groups)
	}
	if len(groups[0].Sources) != 2 {
		t.Fatalf("all source mappings must be retained: %+v", groups[0].Sources)
	}
	if groups[0].Sources[0].Track != model.TrackField || groups[0].Sources[1].Track != model.TrackStudio {
		t.Fatalf("sources must be sorted: %+v", groups[0].Sources)
	}
}

func TestFindDuplicates_TruncatedNeverMergesWithComplete(t *testing.T) {
	data := bytes.Repeat([]byte("Q"), BlockSize*2+37)
	cut := data[:len(data)-100]
	fps := []*Fingerprint{
		mustFP(t, data, src(model.TrackStudio, 0), false),
		mustFP(t, cut, src(model.TrackField, 0), true),
	}
	if g := FindDuplicates(fps); len(g) != 0 {
		t.Fatalf("truncated prefix must not merge with full content: %+v", g)
	}

	// Two equally truncated copies may still group, and only with each other.
	fps = append(fps, mustFP(t, cut, src(model.TrackVoice, 7), true))
	groups := FindDuplicates(fps)
	if len(groups) != 1 || !groups[0].Truncated || len(groups[0].Sources) != 2 {
		t.Fatalf("truncated copies must form their own group: %+v", groups)
	}
}

func TestFingerprint_EmptyAndSingleByte(t *testing.T) {
	empty, err := FingerprintReader(bytes.NewReader(nil), src(model.TrackVoice, 0), false)
	if err != nil {
		t.Fatal(err)
	}
	if empty.Bytes != 0 || len(empty.Blocks) != 0 || empty.DigestHex == "" {
		t.Fatalf("empty fingerprint wrong: %+v", empty)
	}
	one, err := FingerprintReader(bytes.NewReader([]byte{0x01}), src(model.TrackVoice, 1), false)
	if err != nil || len(one.Blocks) != 1 {
		t.Fatalf("single-byte fragment must yield one block: %v", one)
	}
}

func mustFP(t *testing.T, data []byte, s Source, truncated bool) *Fingerprint {
	t.Helper()
	fp, err := FingerprintReader(bytes.NewReader(data), s, truncated)
	if err != nil {
		t.Fatal(err)
	}
	return fp
}
