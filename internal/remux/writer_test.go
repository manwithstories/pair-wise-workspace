package remux

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"mediad/internal/media"
)

func sampleEntryBytes() []byte {
	// A minimal but syntactically valid visual sample entry payload.
	entry := make([]byte, 86)
	binary.BigEndian.PutUint32(entry[0:], uint32(len(entry)))
	copy(entry[4:], "avc1")
	binary.BigEndian.PutUint16(entry[14:], 1)
	binary.BigEndian.PutUint16(entry[32:], 1280)
	binary.BigEndian.PutUint16(entry[34:], 720)
	copy(entry[8+78:], []byte{0x01, 0x64, 0x00, 0x28})
	return entry
}

func makeTracks(n int) []*TrackInput {
	key := media.TrackKey{Camera: "cam1", Language: "und", Codec: "avc1", Role: "main"}
	tr := &TrackInput{Key: key, TrackID: 1, Kind: media.KindVideo, Timescale: 90000, SampleEntry: sampleEntryBytes()}
	for i := 0; i < n; i++ {
		payload := bytes.Repeat([]byte{byte(i), 0xAA, 0xBB, 0xCC}, 64)
		tr.Samples = append(tr.Samples, media.Sample{
			Key:      key,
			DTS:      int64(i) * 3000,
			PTS:      int64(i)*3000 + 3000,
			Duration: 3000,
			Sync:     i%10 == 0,
			Data:     payload,
			Source:   media.SourceRef{Segment: i / 4, Sample: i % 4},
		})
	}
	return []*TrackInput{tr}
}

func TestWritePreservesSampleBytes(t *testing.T) {
	dir := t.TempDir()
	tracks := makeTracks(10)
	wantHashes := map[[32]byte]bool{}
	for _, s := range tracks[0].Samples {
		wantHashes[sha256.Sum256(s.Data)] = true
	}
	sum, idx, err := Write(dir, tracks, Options{})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if !sum.SourceHashMatch {
		t.Fatal("source hash match false")
	}
	if sum.OutputSamples != 10 {
		t.Fatalf("output samples=%d", sum.OutputSamples)
	}
	data, err := os.ReadFile(filepath.Join(dir, "output.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	for s := range wantHashes {
		found := false
		for _, sample := range tracks[0].Samples {
			if sha256.Sum256(sample.Data) == s && bytes.Contains(data, sample.Data) {
				found = true
				break
			}
		}
		if !found {
			t.Fatal("a source sample's bytes are missing from output")
		}
	}
	if len(idx.Entries) != 10 {
		t.Fatalf("index entries=%d", len(idx.Entries))
	}
}

func TestRepeatedRunsIdentical(t *testing.T) {
	dir1, dir2 := t.TempDir(), t.TempDir()
	s1, _, err := Write(dir1, makeTracks(12), Options{})
	if err != nil {
		t.Fatal(err)
	}
	s2, _, err := Write(dir2, makeTracks(12), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if s1.OutputSHA256 != s2.OutputSHA256 {
		t.Fatalf("output differs between runs:\n%s\n%s", s1.OutputSHA256, s2.OutputSHA256)
	}
	if s1.SampleSHA256 != s2.SampleSHA256 {
		t.Fatal("sample digest differs between runs")
	}
}

func TestOutputSizeLimitRemovesTemp(t *testing.T) {
	dir := t.TempDir()
	_, _, err := Write(dir, makeTracks(20), Options{MaxOutputBytes: 256})
	if err == nil {
		t.Fatal("expected limit error")
	}
	if media.CodeOf(err) != media.CodeLimit {
		t.Fatalf("code=%s", media.CodeOf(err))
	}
	assertNoOutput(t, dir)
}

func TestMissingSampleEntryRejected(t *testing.T) {
	dir := t.TempDir()
	tracks := makeTracks(3)
	tracks[0].SampleEntry = nil
	if _, _, err := Write(dir, tracks, Options{}); err == nil {
		t.Fatal("expected integrity error for missing sample entry")
	}
	assertNoOutput(t, dir)
}

func TestVerifyDetectsCorruption(t *testing.T) {
	dir := t.TempDir()
	p := &plan{tracks: makeTracks(2)}
	bad := filepath.Join(dir, "bad.mp4")
	// A truncated box header must be reported, not accepted.
	if err := os.WriteFile(bad, []byte{0, 0, 0, 100, 'f', 't', 'y', 'p'}, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := verify(bad, p); err == nil {
		t.Fatal("expected truncation error from verify")
	} else if media.CodeOf(err) != media.CodeTruncatedBox {
		t.Fatalf("code=%s", media.CodeOf(err))
	}
}

func TestEmptyTrackRejected(t *testing.T) {
	dir := t.TempDir()
	tr := &TrackInput{
		Key:         media.TrackKey{Camera: "cam1", Role: "main"},
		TrackID:     1,
		Timescale:   90000,
		SampleEntry: sampleEntryBytes(),
	}
	if _, _, err := Write(dir, []*TrackInput{tr}, Options{}); err == nil {
		t.Fatal("expected error for track with no samples")
	}
	assertNoOutput(t, dir)
}

func assertNoOutput(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() == "output.mp4" {
			t.Fatalf("corrupt/limited run left output.mp4 behind")
		}
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Fatalf("temp file %q left behind", e.Name())
		}
	}
}
