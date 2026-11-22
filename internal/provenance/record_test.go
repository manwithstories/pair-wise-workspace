package provenance

import (
	"path/filepath"
	"testing"
)

func TestDigestDeterministic(t *testing.T) {
	r := Record{
		Output:               "report",
		SourceManifestDigest: "abc",
		SegmentRange:         [2]int{1, 5},
		TimeBase:             "1/90000",
		AlgoVersion:          "1.0.0",
		ParamsDigest:         "p",
		ErrorChain:           []string{"TRUNCATED_SEGMENT@v1/3"},
	}
	if r.Digest() != r.Digest() {
		t.Fatal("digest not deterministic")
	}
	r2 := r
	r2.SegmentRange[1] = 6
	if r.Digest() == r2.Digest() {
		t.Fatal("different records must have different digests")
	}
}

func TestAppendAndRead(t *testing.T) {
	log := Open(filepath.Join(t.TempDir(), "prov", "journal.jsonl"))
	r1 := Record{Output: "a", SegmentRange: [2]int{0, 1}, ErrorChain: []string{}}
	r2 := Record{Output: "b", SegmentRange: [2]int{1, 2}, ErrorChain: []string{"x"}}
	if err := log.Append(r1); err != nil {
		t.Fatal(err)
	}
	if err := log.Append(r2); err != nil {
		t.Fatal(err)
	}
	records, err := log.Records()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 || records[0].Output != "a" || records[1].ErrorChain[0] != "x" {
		t.Fatalf("bad journal: %+v", records)
	}
}

func TestRecordsMissingFile(t *testing.T) {
	log := Open(filepath.Join(t.TempDir(), "none.jsonl"))
	records, err := log.Records()
	if err != nil || records != nil {
		t.Fatalf("missing journal should be empty: %v %v", records, err)
	}
}
