package scan

import (
	"bytes"
	"strings"
	"testing"

	"medpipe/internal/model"
)

func TestBuildAndValidateIndex(t *testing.T) {
	raw := buildContainer(t, 8)
	res := scanBytes(t, raw)
	digest, size, err := DigestSource(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	idx := BuildIndex(res, digest, size)
	if err := idx.Validate(); err != nil {
		t.Fatalf("valid index rejected: %v", err)
	}
	if idx.FrameCount != 8 || idx.AudioCount != 4 || len(idx.Frames) != 8 {
		t.Fatalf("index counts wrong: %+v", idx)
	}
	if idx.Frames[0].Offset != int64(model.HeaderSize) {
		t.Fatalf("first frame offset %d", idx.Frames[0].Offset)
	}
	if idx.Frames[0].EndNS <= idx.Frames[0].StartNS {
		t.Fatal("end timestamp must exceed start")
	}
}

func TestIndexValidateRejectsIncomplete(t *testing.T) {
	raw := buildContainer(t, 4)
	res := scanBytes(t, raw)
	idx := BuildIndex(res, "abc", int64(len(raw)))
	if err := idx.Validate(); err == nil ||
		!strings.Contains(err.Error(), string(model.ErrIndexIncomplete)) {
		t.Fatalf("short digest must be rejected, got %v", err)
	}

	good, size, _ := DigestSource(bytes.NewReader(raw))
	idx = BuildIndex(res, good, size)
	idx.Frames = idx.Frames[:2] // frame_count no longer matches entries
	if err := idx.Validate(); err == nil {
		t.Fatal("count mismatch must be rejected")
	}

	idx = BuildIndex(res, good, size)
	idx.Frames[1].Digest = ""
	if err := idx.Validate(); err == nil {
		t.Fatal("missing per-frame digest must be rejected")
	}
}

func TestDigestDeterministic(t *testing.T) {
	raw := buildContainer(t, 3)
	d1, s1, _ := DigestSource(bytes.NewReader(raw))
	d2, s2, _ := DigestSource(bytes.NewReader(raw))
	if d1 != d2 || s1 != s2 {
		t.Fatal("digest not deterministic")
	}
}
