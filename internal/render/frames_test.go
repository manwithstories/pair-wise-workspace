package render

import (
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"

	"archive/internal/model"
)

func sampleFrames(n int) []FrameRecord {
	out := make([]FrameRecord, n)
	for i := range out {
		h := sha256.Sum256([]byte{byte(i), byte(i >> 8)})
		out[i] = FrameRecord{
			Index: int64(i), Track: model.TrackVideo, TimeNanos: int64(i) * 10_000_000,
			SourceStart: int64(i), SourceEnd: int64(i), SSRC: 1,
			Keyframe: i%25 == 0, PayloadHash: h, ClockBasis: "rtcp-sr",
		}
	}
	return out
}

// TestSegmentHashMatchesDisk verifies the reported segment hash equals the
// hash of the file an independent reader sees (regression for go-mp4 size
// patching during write).
func TestSegmentHashMatchesDisk(t *testing.T) {
	dir := t.TempDir()
	w := NewSegmentWriter(dir, 0, 0, 0)
	for _, f := range sampleFrames(50) {
		w.Add(f)
	}
	seg, err := w.Flush()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, segmentName(0)))
	if err != nil {
		t.Fatal(err)
	}
	if got := sha256.Sum256(data); got != seg.SHA256 {
		t.Error("segment SHA256 does not match on-disk bytes")
	}
	if seg.Frames != 50 {
		t.Errorf("frames = %d, want 50", seg.Frames)
	}
}

// TestSegmentDeterministic verifies identical frames produce identical
// content hashes, while distinct segment sequence numbers make the
// whole-file hashes unique as required by fragmented MP4.
func TestSegmentDeterministic(t *testing.T) {
	dir := t.TempDir()
	w1 := NewSegmentWriter(dir, 1, 0, 0)
	for _, f := range sampleFrames(30) {
		w1.Add(f)
	}
	s1, err := w1.Flush()
	if err != nil {
		t.Fatal(err)
	}
	w2 := NewSegmentWriter(dir, 2, 0, 0)
	for _, f := range sampleFrames(30) {
		w2.Add(f)
	}
	s2, err := w2.Flush()
	if err != nil {
		t.Fatal(err)
	}
	if s1.ContentSHA != s2.ContentSHA {
		t.Error("identical frame sets must produce identical content hashes")
	}
	if s1.SHA256 == s2.SHA256 {
		t.Error("distinct sequence numbers must produce distinct whole-file hashes")
	}
}

func TestEmptySegmentRejected(t *testing.T) {
	w := NewSegmentWriter(t.TempDir(), 0, 0, 0)
	if _, err := w.Flush(); err == nil {
		t.Error("flushing an empty segment must error")
	}
}
