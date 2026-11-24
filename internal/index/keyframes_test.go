package index

import (
	"testing"

	"media-check/internal/model"
)

func frames(n, gop int) []model.Frame {
	out := make([]model.Frame, n)
	for i := range out {
		out[i] = model.Frame{
			Seq: uint32(i), Time: int64(i * 3000), Keyframe: i%gop == 0,
		}
	}
	return out
}

func TestBuildAndVerify(t *testing.T) {
	fs := frames(60, 15)
	idx := Build(fs)
	if len(idx.Positions) != 4 {
		t.Fatalf("positions=%v", idx.Positions)
	}
	hits, total := Verify(idx, fs)
	if hits != total || total != 4 {
		t.Fatalf("hits=%d total=%d", hits, total)
	}
}

func TestFromSampleTableMatchesBuild(t *testing.T) {
	fs := frames(60, 15)
	var keys []uint32
	for i, f := range fs {
		if f.Keyframe {
			keys = append(keys, uint32(i+1))
		}
	}
	idx := FromSampleTable(keys, 3000, 0)
	hits, total := Verify(idx, fs)
	if hits != total {
		t.Fatalf("stss-derived index misses: %d/%d", hits, total)
	}
}

func TestVerifyDetectsDrift(t *testing.T) {
	fs := frames(30, 10)
	idx := Build(fs)
	idx.Times[1] += 1 // corrupt one indexed time
	hits, total := Verify(idx, fs)
	if hits != total-1 {
		t.Fatalf("drift not detected: %d/%d", hits, total)
	}
}
