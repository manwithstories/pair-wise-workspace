package pipeline

import (
	"testing"

	"archive.local/recover/internal/index"
	"archive.local/recover/internal/media"
	"archive.local/recover/internal/stream"
	"archive.local/recover/internal/testkit"
)

// TestCorruptIndexDropsOnlyOutliers pins the repair policy: one bad page must
// not cause every page after it to be discarded.
func TestCorruptIndexDropsOnlyOutliers(t *testing.T) {
	fx, err := testkit.Generate(testkit.Options{
		Seed: 17, DurationNs: 120e9, Bitrates: []int{800}, Loss: 0.03, Case: testkit.CaseCorruptIndex,
	})
	if err != nil {
		t.Fatal(err)
	}
	rd := &fx.Renditions[0]
	pf, err := media.ParseFile(media.Reader(rd.Master), int64(len(rd.Master)))
	if err != nil {
		t.Fatal(err)
	}
	scan, err := stream.Scan(stream.H264, pf.Data)
	if err != nil {
		t.Fatal(err)
	}
	p := index.Parse(rd.Index, "idx")
	res := index.Validate(&p.Parsed, scan, "idx")
	// The fixture corrupts one page and deletes another; at most a couple of
	// entries should be rejected, never a majority.
	if res.Dropped > 2 {
		t.Fatalf("dropped %d of %d entries for one corrupt page", res.Dropped, len(p.Entries))
	}
	if res.Repaired == 0 || res.Missing == 0 {
		t.Fatalf("missing page not recovered: %+v", res)
	}
	for i := 1; i < len(res.Entries); i++ {
		a, b := res.Entries[i-1], res.Entries[i]
		if a.Offset >= 0 && b.Offset >= 0 && b.Offset < a.Offset {
			t.Fatalf("repaired index is out of order at %d: %+v", i, res.Entries)
		}
	}
}
