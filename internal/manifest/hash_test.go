package manifest

import (
	"testing"

	"media-check/internal/model"
)

func TestHashFramesDeterministic(t *testing.T) {
	fs := []model.Frame{
		{Seq: 1, Time: 0, Keyframe: true, Payload: []byte("abc")},
		{Seq: 2, Time: 3000, Payload: []byte("def")},
	}
	if HashFrames(fs) != HashFrames(fs) {
		t.Fatal("hash not deterministic")
	}
	fs2 := []model.Frame{fs[0], fs[1]}
	fs2[1].Payload = []byte("deg")
	if HashFrames(fs) == HashFrames(fs2) {
		t.Fatal("payload change must change hash")
	}
	fs3 := []model.Frame{fs[0], fs[1]}
	fs3[1].Time++
	if HashFrames(fs) == HashFrames(fs3) {
		t.Fatal("time change must change hash")
	}
}

func TestSegmentEntryMatch(t *testing.T) {
	e := SegmentEntry{SrcHash: HashBytes([]byte("x")), DstHash: HashBytes([]byte("x"))}
	if !e.HashMatch() {
		t.Fatal("equal hashes must match")
	}
	e.DstHash = HashBytes([]byte("y"))
	if e.HashMatch() {
		t.Fatal("different hashes must not match")
	}
}
