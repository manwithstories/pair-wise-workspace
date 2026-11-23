package manifest

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"mediad/internal/index"
	"mediad/internal/model"
)

func frame(no uint64, off int64, capture uint64, captureUPS, timescale uint32) model.Frame {
	return model.Frame{
		Meta: model.FrameMeta{
			TrackID: 1, FrameNo: no, CaptureUnits: capture,
			CaptureUPS: captureUPS, Timescale: timescale,
		},
		Offset: off, Size: 100, Status: model.FrameOK,
		PTS:    int64(capture-1000) * int64(timescale) / int64(captureUPS),
		Digest: model.DigestBytes([]byte{byte(no)}),
	}
}

func entryOf(fr model.Frame) index.Entry {
	return index.Entry{
		FrameNo: fr.Meta.FrameNo, TrackID: 1, PTS: fr.PTS,
		Offset: fr.Offset, Size: fr.Size,
		Digest: index.EntryDigest(1, fr.Meta.FrameNo, fr.PTS, fr.Offset, fr.Size, fr.Digest),
	}
}

func TestBuildWritesHeaderAndRows(t *testing.T) {
	dir := t.TempDir()
	var entries []index.Entry
	var frames []model.Frame
	for i := uint64(1); i <= 5; i++ {
		fr := frame(i, int64(i)*1000, 1000+i-1, 1000, 90000)
		frames = append(frames, fr)
		entries = append(entries, entryOf(fr))
	}
	path := filepath.Join(dir, "manifest.csv")
	d1, rows, err := Build(path, entries, frames)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if rows != 5 {
		t.Fatalf("rows = %d", rows)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	d2, err := DigestReader(bytesReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if d1 != d2 {
		t.Fatal("manifest digest is not stable over its bytes")
	}
	// Rebuilding identical input yields identical bytes and digest.
	d3, _, err := Build(path, entries, frames)
	if err != nil {
		t.Fatal(err)
	}
	if d3 != d1 {
		t.Fatal("manifest digest changed across identical builds")
	}
}

func bytesReader(b []byte) *bytes.Reader { return bytes.NewReader(b) }

func TestAlignAcceptsConsistentManifest(t *testing.T) {
	const ups, ts = 1000, 90000
	var frames []model.Frame
	var entries []index.Entry
	for i := uint64(0); i < 50; i++ {
		fr := frame(i+1, int64(i)*1000, 1000+i, ups, ts)
		frames = append(frames, fr)
		entries = append(entries, entryOf(fr))
	}
	rep, err := Align(Args{
		Frames: frames, Entries: entries, Timescale: ts, CaptureUPS: ups,
		EpochUnits: 1000,
	})
	if err != nil {
		t.Fatalf("align: %v", err)
	}
	if rep.Rows != 50 || rep.SkewMS > MaxSkewMS {
		t.Fatalf("report %+v", rep)
	}
}

func TestAlignRejectsEntryPTSThatDiffers(t *testing.T) {
	const ups, ts = 1000, 90000
	var frames []model.Frame
	var entries []index.Entry
	for i := uint64(0); i < 5; i++ {
		fr := frame(i+1, int64(i)*1000, 1000+i, ups, ts)
		frames = append(frames, fr)
		e := entryOf(fr)
		if i == 2 {
			e.PTS += 5000 // stale index entry
		}
		entries = append(entries, e)
	}
	if _, err := Align(Args{Frames: frames, Entries: entries, Timescale: ts, CaptureUPS: ups, EpochUnits: 1000}); err == nil {
		t.Fatal("align accepted an entry whose PTS differs from the timeline")
	}
}

func TestAlignRejectsGapInfringement(t *testing.T) {
	const ups, ts = 1000, 90000
	var frames []model.Frame
	var entries []index.Entry
	for i := uint64(0); i < 5; i++ {
		fr := frame(i+1, int64(i)*1000, 1000+i, ups, ts)
		frames = append(frames, fr)
		entries = append(entries, entryOf(fr))
	}
	_, err := Align(Args{
		Frames: frames, Entries: entries, Timescale: ts, CaptureUPS: ups, EpochUnits: 1000,
		Missing: []Gap{{From: 3, To: 3}},
	})
	if err == nil {
		t.Fatal("align accepted an entry inside a known gap")
	}
}

func TestAlignRejectsMissingRow(t *testing.T) {
	const ups, ts = 1000, 90000
	var frames []model.Frame
	var entries []index.Entry
	for i := uint64(0); i < 5; i++ {
		fr := frame(i+1, int64(i)*1000, 1000+i, ups, ts)
		frames = append(frames, fr)
		entries = append(entries, entryOf(fr))
	}
	if _, err := Align(Args{Frames: frames, Entries: entries[:4], Timescale: ts, CaptureUPS: ups, EpochUnits: 1000}); err == nil {
		t.Fatal("align accepted a manifest missing a frame")
	}
}
