package segment

import "testing"

func TestBuildIndexGapsAndOverlap(t *testing.T) {
	segments := []Segment{
		{Rendition: 0, Seq: 0, SourceTS: 0, DeclaredMS: 4000, Size: 200000, MeasuredMS: 4000, FileName: "a"},
		{Rendition: 0, Seq: 1, SourceTS: 4000, DeclaredMS: 4000, Size: 200000, MeasuredMS: 4000, FileName: "b"},
		{Rendition: 0, Seq: 4, SourceTS: 16000, DeclaredMS: 4000, Size: 200000, MeasuredMS: 4000, FileName: "e"},
	}
	index := BuildIndex(segments, 4000)
	rIndex := index.Renditions[0]
	if got := rIndex.GapCount(); got != 2 {
		t.Fatalf("gap count = %d, want 2", got)
	}
	if len(rIndex.Gaps) != 1 || rIndex.Gaps[0].FromSeq != 2 || rIndex.Gaps[0].ToSeq != 3 {
		t.Fatalf("unexpected gap interval: %+v", rIndex.Gaps)
	}
	if rIndex.MaxSeq != 4 {
		t.Fatalf("max seq = %d, want 4", rIndex.MaxSeq)
	}
}

func TestBuildIndexZeroByteFailure(t *testing.T) {
	segments := []Segment{
		{Rendition: 1, Seq: 0, SourceTS: 0, DeclaredMS: 4000, Size: 0, MeasuredMS: 0, FileName: "z"},
	}
	index := BuildIndex(segments, 4000)
	if len(index.Renditions[1].ZeroByte) != 1 {
		t.Fatalf("zero byte not recorded on rendition index")
	}
	found := false
	for _, failure := range index.Failures {
		if failure.Kind == "zero_byte" && failure.File == "z" {
			found = true
		}
	}
	if !found {
		t.Fatalf("zero byte failure missing from index failures: %+v", index.Failures)
	}
}
