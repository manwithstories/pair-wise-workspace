package manifest

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	"mediad/internal/index"
	"mediad/internal/model"
)

func entryOf(no uint64, pts, off int64) index.Entry {
	content := model.DigestBytes([]byte{byte(no)})
	return index.Entry{
		FrameNo: no, TrackID: 1, PTS: pts, Offset: off, Size: 100,
		Digest: index.EntryDigest(1, no, pts, off, 100, content),
	}
}

func entryIter(entries []index.Entry) func() (index.Entry, bool, error) {
	i := 0
	return func() (index.Entry, bool, error) {
		if i >= len(entries) {
			return index.Entry{}, false, nil
		}
		e := entries[i]
		i++
		return e, true, nil
	}
}

type frames struct {
	items []FrameRef
	i     int
}

func (f *frames) Next() (FrameRef, error) {
	if f.i >= len(f.items) {
		return FrameRef{}, io.EOF
	}
	fr := f.items[f.i]
	f.i++
	return fr, nil
}

func TestBuildStreamsRowsAndDigest(t *testing.T) {
	dir := t.TempDir()
	entries := []index.Entry{entryOf(1, 0, 0), entryOf(2, 90, 1000), entryOf(3, 180, 2000)}
	path := filepath.Join(dir, "m.csv")
	digest, rows, err := Build(path, entryIter(entries))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if rows != 3 {
		t.Fatalf("rows = %d", rows)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	d2, err := DigestReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if digest != d2 {
		t.Fatal("digest does not match written bytes")
	}
	// A rebuild is byte-identical.
	path2 := filepath.Join(dir, "m2.csv")
	d3, _, err := Build(path2, entryIter(entries))
	if err != nil {
		t.Fatal(err)
	}
	if d3 != digest {
		t.Fatal("rebuild produced a different digest")
	}
}

func TestAlignAcceptsConsistent(t *testing.T) {
	const ups, ts = 1000, 90000
	var entries []index.Entry
	var frs []FrameRef
	for i := uint64(0); i < 50; i++ {
		pts := int64(i) * ts / ups
		entries = append(entries, entryOf(i+1, pts, int64(i)*1000))
		frs = append(frs, FrameRef{FrameNo: i + 1, PTS: pts, CaptureUnits: 1000 + i, Usable: true})
	}
	rep, err := Align(Args{
		Entries: entryIter(entries), Frames: &frames{items: frs},
		Timescale: ts, CaptureUPS: ups, EpochUnits: 1000,
	})
	if err != nil {
		t.Fatalf("align: %v", err)
	}
	if rep.Rows != 50 {
		t.Fatalf("report %+v", rep)
	}
}

func TestAlignRejectsPTSMismatch(t *testing.T) {
	const ups, ts = 1000, 90000
	entries := []index.Entry{entryOf(1, 0, 0), entryOf(2, 5000, 1000)}
	frs := []FrameRef{
		{FrameNo: 1, PTS: 0, CaptureUnits: 1000, Usable: true},
		{FrameNo: 2, PTS: 90, CaptureUnits: 1001, Usable: true},
	}
	if _, err := Align(Args{Entries: entryIter(entries), Frames: &frames{items: frs}, Timescale: ts, CaptureUPS: ups, EpochUnits: 1000}); err == nil {
		t.Fatal("accepted an entry whose PTS differs from the timeline")
	}
}

func TestAlignRejectsGapInfringement(t *testing.T) {
	const ups, ts = 1000, 90000
	entries := []index.Entry{entryOf(1, 0, 0), entryOf(3, 180, 2000)}
	frs := []FrameRef{
		{FrameNo: 1, PTS: 0, CaptureUnits: 1000, Usable: true},
		{FrameNo: 3, PTS: 180, CaptureUnits: 1002, Usable: true},
	}
	if _, err := Align(Args{
		Entries: entryIter(entries), Frames: &frames{items: frs},
		Timescale: ts, CaptureUPS: ups, EpochUnits: 1000,
		Missing: []Gap{{From: 3, To: 3}},
	}); err == nil {
		t.Fatal("accepted an entry inside a known gap")
	}
}

func TestAlignRejectsMissingRow(t *testing.T) {
	const ups, ts = 1000, 90000
	var entries []index.Entry
	var frs []FrameRef
	for i := uint64(0); i < 5; i++ {
		pts := int64(i) * 90
		entries = append(entries, entryOf(i+1, pts, int64(i)*1000))
		frs = append(frs, FrameRef{FrameNo: i + 1, PTS: pts, CaptureUnits: 1000 + i, Usable: true})
	}
	if _, err := Align(Args{
		Entries: entryIter(entries[:4]), Frames: &frames{items: frs},
		Timescale: ts, CaptureUPS: ups, EpochUnits: 1000,
	}); err == nil {
		t.Fatal("accepted a manifest missing a frame")
	}
}

func TestAlignRejectsNonMonotonicTimeline(t *testing.T) {
	const ups, ts = 1000, 90000
	entries := []index.Entry{entryOf(1, 0, 0), entryOf(2, 5000, 1000)}
	// Source frames themselves go backwards: a broken unified timeline.
	frs := []FrameRef{
		{FrameNo: 1, PTS: 0, CaptureUnits: 1000, Usable: true},
		{FrameNo: 2, PTS: -90, CaptureUnits: 1001, Usable: true},
	}
	if _, err := Align(Args{Entries: entryIter(entries), Frames: &frames{items: frs}, Timescale: ts, CaptureUPS: ups, EpochUnits: 1000}); err == nil {
		t.Fatal("accepted a non-monotonic timeline")
	}
}
