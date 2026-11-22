package index

import (
	"testing"

	"mediascan/internal/media"
	"mediascan/internal/stream"
)

// fakeTrack builds a track whose samples are keyframes at the given numbers.
func fakeTrack(keyframes []int, n int) *stream.Track {
	tr := &stream.Track{TrackID: 1, Kind: "video", Timescale: 30000, Complete: true}
	sync := map[int]bool{}
	for _, k := range keyframes {
		sync[k] = true
	}
	for i := 1; i <= n; i++ {
		tr.Samples = append(tr.Samples, stream.Sample{
			Number: i, DTSNs: int64(i-1) * 33_366_666, EndNs: int64(i) * 33_366_666,
			Sync: sync[i],
		})
	}
	return tr
}

func TestBuildHealthyIndex(t *testing.T) {
	tr := fakeTrack([]int{1, 11, 21}, 30)
	tr.IndexSource = "stss"
	tr.Keyframes = []int{1, 11, 21}
	res := Build(tr)
	if res.Rebuilt {
		t.Error("healthy index was rebuilt")
	}
	if res.Index.Source != "stss" || !res.Index.Complete {
		t.Errorf("index = %+v", res.Index)
	}
	if len(res.Issues) != 0 {
		t.Errorf("unexpected issues: %v", res.Issues)
	}
	if hr := res.Index.HitRate(tr); hr != 1.0 {
		t.Errorf("hit rate = %v, want 1.0", hr)
	}
	iv := res.Index.Intervals(tr)
	if len(iv) != 2 {
		t.Fatalf("intervals = %v", iv)
	}
	// GOP of 10 frames at 29.97 = 333_666_660 ns.
	if iv[0] != 333_666_660 {
		t.Errorf("interval = %d, want 333666660", iv[0])
	}
}

func TestBuildMissingIndexRebuilds(t *testing.T) {
	// Index source stss but no keyframes declared while samples say 1,11,21
	// are sync: the declared index is incomplete and must be rebuilt.
	tr := fakeTrack([]int{1, 11, 21}, 30)
	tr.IndexSource = "stss"
	tr.Keyframes = nil
	res := Build(tr)
	if !res.Rebuilt {
		t.Fatal("incomplete index not rebuilt")
	}
	if res.Index.Source != "rebuilt" {
		t.Errorf("source = %q", res.Index.Source)
	}
	if len(res.Index.Synced) != 3 {
		t.Errorf("rebuilt keyframes = %v", res.Index.Synced)
	}
	if !hasClass(res.Issues, media.ClassMP4IndexIncomplete) {
		t.Errorf("incomplete index not reported: %v", res.Issues)
	}
	if hr := res.Index.HitRate(tr); hr != 1.0 {
		t.Errorf("hit rate = %v", hr)
	}
}

func TestBuildNoKeyframesDistinctFromIncomplete(t *testing.T) {
	// Samples carry no sync flag at all: this is "no keyframes", a different
	// condition from an incomplete index.
	tr := fakeTrack(nil, 10)
	tr.IndexSource = "stss"
	tr.Keyframes = nil
	res := Build(tr)
	if !res.Rebuilt {
		t.Fatal("expected rebuild scan")
	}
	if !hasClass(res.Issues, media.ClassMP4NoKeyframes) {
		t.Fatalf("no-keyframes not reported: %v", res.Issues)
	}
	// It must not also claim the index was merely incomplete.
	if hasClass(res.Issues, media.ClassMP4IndexIncomplete) {
		t.Errorf("no-keyframes misreported as incomplete index: %v", res.Issues)
	}
	if hr := res.Index.HitRate(tr); hr != 0 {
		t.Errorf("hit rate = %v, want 0", hr)
	}
}

func TestBuildInvalidSampleNumbers(t *testing.T) {
	tr := fakeTrack([]int{1, 11}, 20)
	tr.IndexSource = "stss"
	tr.Keyframes = []int{1, 99} // 99 is out of range
	res := Build(tr)
	if !hasClass(res.Issues, media.ClassMP4IndexIncomplete) {
		t.Errorf("out-of-range stss entry not reported: %v", res.Issues)
	}
	if !res.Rebuilt {
		t.Error("invalid index not rebuilt")
	}
}

func TestBuildImplicitAllSync(t *testing.T) {
	tr := fakeTrack(nil, 5)
	tr.IndexSource = "implicit-all-sync"
	for i := range tr.Samples {
		tr.Samples[i].Sync = true
	}
	res := Build(tr)
	if res.Index.Source != "implicit" || len(res.Index.Synced) != 5 {
		t.Errorf("index = %+v", res.Index)
	}
	if len(res.Issues) != 0 {
		t.Errorf("unexpected issues: %v", res.Issues)
	}
}

// TestPropagationCorruptSampleTriggersRebuild is the cross-module path: a
// damaged sample discovered by the MP4 analyzer must flow into the indexer as
// an index rebuild, not a whole-track failure.
func TestPropagationCorruptSampleTriggersRebuild(t *testing.T) {
	tr := fakeTrack([]int{1, 11}, 20)
	// Simulate the analyzer having lost the declared index because a portion
	// of the file was corrupt: stss is absent, samples still mark sync.
	tr.IndexSource = "stss"
	tr.Keyframes = nil
	tr.Complete = false
	res := Build(tr)
	if !res.Rebuilt || res.Index.Source != "rebuilt" {
		t.Fatalf("corruption did not trigger rebuild: %+v", res)
	}
	if len(res.Index.Synced) != 2 {
		t.Errorf("rebuilt keyframes = %v, want 2", res.Index.Synced)
	}
	if res.Index.Complete {
		t.Error("incomplete track index marked complete")
	}
	if hr := res.Index.HitRate(tr); hr != 1.0 {
		t.Errorf("hit rate = %v", hr)
	}
}

func hasClass(set media.IssueSet, c media.Class) bool {
	for _, is := range set {
		if is.Class == c {
			return true
		}
	}
	return false
}
