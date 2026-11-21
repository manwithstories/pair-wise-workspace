package index

import (
	"os"
	"testing"

	"mediascan/internal/fixture"
	"mediascan/internal/manifest"
)

func prep(t *testing.T) (*manifest.Resolver, *manifest.Timeline, string) {
	work := t.TempDir()
	if _, err := fixture.Generate(work); err != nil {
		t.Fatal(err)
	}
	res, _ := manifest.NewResolver(work)
	tl, err := manifest.Load(res, "hls/media.m3u8")
	if err != nil {
		t.Fatal(err)
	}
	manifest.MarkPresence(res, tl)
	return res, tl, work
}

func TestScanStopAfterIsResumable(t *testing.T) {
	res, tl, work := prep(t)
	idx, err := Scan(res, tl, Options{StopAfter: 3})
	if err != ErrInterrupted || idx == nil {
		t.Fatalf("want ErrInterrupted with partial index, got %v", err)
	}
	if idx.Complete || len(idx.Entries) != 3 {
		t.Fatalf("partial index: complete=%v entries=%d", idx.Complete, len(idx.Entries))
	}
	prev := idx
	full, err := Scan(res, tl, Options{ResumeFrom: 3, Prev: prev})
	if err != nil {
		t.Fatal(err)
	}
	if full.Skipped != 3 {
		t.Fatalf("skipped=%d", full.Skipped)
	}
	if full.InputSHA != prev.InputSHA {
		t.Fatal("digest changed")
	}
	if len(full.Entries) != fixture.SegmentCount {
		t.Fatalf("entries=%d", len(full.Entries))
	}
	all, _ := Scan(res, tl, Options{})
	if all.InputSHA != full.InputSHA {
		t.Fatal("input digest unstable")
	}
	for i := range all.Entries {
		if all.Entries[i].SHA256 != full.Entries[i].SHA256 {
			t.Fatalf("entry %d digest mismatch after resume", i)
		}
	}
	_ = work
}

func TestMissingSegmentRetainsPartialTimeline(t *testing.T) {
	res, tl, _ := prep(t)
	idx, err := Scan(res, tl, Options{})
	if err != nil {
		t.Fatal(err)
	}
	var missing *Entry
	for i := range idx.Entries {
		if !idx.Entries[i].Present {
			missing = &idx.Entries[i]
			break
		}
	}
	if missing == nil {
		t.Fatal("no missing entry recorded")
	}
	if missing.Name != "seg-07.m4s" {
		t.Fatalf("missing=%s", missing.Name)
	}
	if idx.FirstMiss != "media/seg-07.m4s" {
		t.Fatalf("first miss=%s", idx.FirstMiss)
	}
	// Every other segment is still indexed: the timeline is partial, not empty.
	if idx.Scanned != fixture.SegmentCount-1 {
		t.Fatalf("scanned=%d", idx.Scanned)
	}
}

func TestByteRangeBoundsRejected(t *testing.T) {
	work := t.TempDir()
	if _, err := fixture.Generate(work); err != nil {
		t.Fatal(err)
	}
	res, _ := manifest.NewResolver(work)
	body := "#EXTM3U\n#EXTINF:2,\n#EXT-X-BYTERANGE:999999999@0\nmedia/seg-01.m4s\n"
	tl, err := manifest.ParseHLS(res, "x.m3u8", []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	manifest.MarkPresence(res, tl)
	idx, err := Scan(res, tl, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(idx.Entries) != 1 || idx.Entries[0].Present {
		t.Fatalf("out-of-range byte range accepted: %+v", idx.Entries)
	}
}

func TestHugeFileRejected(t *testing.T) {
	work := t.TempDir()
	if _, err := fixture.Generate(work); err != nil {
		t.Fatal(err)
	}
	big := make([]byte, MaxSegmentBytes+1)
	if err := os.WriteFile(fixture.SegmentPath(work, 0), big, 0o644); err != nil {
		t.Fatal(err)
	}
	res, _ := manifest.NewResolver(work)
	tl, _ := manifest.Load(res, "hls/media.m3u8")
	manifest.MarkPresence(res, tl)
	idx, err := Scan(res, tl, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if idx.Entries[0].Present {
		t.Fatal("oversized media accepted")
	}
}
