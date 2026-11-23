package media

import (
	"errors"
	"fmt"
	"testing"
)

func TestTimeConvertAndCompare(t *testing.T) {
	a := Time{Value: 90000, Timescale: 90000}  // 1s @90k
	b := Time{Value: 1000, Timescale: 1000}    // 1s @1k
	c := Time{Value: 48000, Timescale: 48000}  // 1s @48k
	if a.Compare(b) != 0 || a.Compare(c) != 0 {
		t.Fatalf("equal times on different bases must compare equal")
	}
	if got := a.ToTimescale(48000); got.Value != 48000 {
		t.Fatalf("ToTimescale got %v", got)
	}
	half := Time{Value: 500, Timescale: 1000}
	if half.Compare(b) != -1 || b.Compare(half) != 1 {
		t.Fatalf("ordering broken")
	}
	if got := half.Add(500); got.Value != 1000 {
		t.Fatalf("Add got %v", got)
	}
}

func TestZeroTimescaleRejected(t *testing.T) {
	if _, err := NewTime(1, 0); err == nil {
		t.Fatal("expected error for zero timescale")
	}
}

func TestErrorModel(t *testing.T) {
	rec := Recoverable("demux.parse", 4096, ErrTruncated)
	if ClassOf(rec) != ClassRecoverable {
		t.Fatal("class mismatch")
	}
	if got := rec.Error(); got == "" {
		t.Fatal("empty message")
	}
	fatal := Fatal("demux.parse", ErrMissingMoov)
	if ClassOf(fmt.Errorf("wrap: %w", fatal)) != ClassFatal {
		t.Fatal("wrapped fatal class mismatch")
	}
	blk := Blocked("index.build", "demux.samples", ErrIncompleteIndex)
	if FirstBlockingDep(blk) != "demux.samples" {
		t.Fatalf("blocking dep = %q", FirstBlockingDep(blk))
	}
	if FirstBlockingDep(errors.New("x")) != "" {
		t.Fatal("plain error must not report a dependency")
	}
	if !errors.Is(rec, ErrTruncated) {
		t.Fatal("Unwrap broken")
	}
}

func TestFileHelpers(t *testing.T) {
	f := &File{
		Tracks: []Track{{ID: 1, Kind: TrackVideo, Timescale: 90000}, {ID: 2, Kind: TrackAudio, Timescale: 48000}},
		Samples: []Sample{
			{TrackID: 1, DTS: Time{Value: 0, Timescale: 90000}},
			{TrackID: 2, DTS: Time{Value: 0, Timescale: 48000}},
				{TrackID: 1, DTS: Time{Value: 3000, Timescale: 90000}},
		},
	}
	tr, ok := f.TrackByKind(TrackAudio)
	if !ok || tr.ID != 2 {
		t.Fatal("TrackByKind failed")
	}
	if n := len(f.SamplesOf(1)); n != 2 {
		t.Fatalf("SamplesOf got %d", n)
	}
	s := f.Samples[0]
	s.Duration = 3000
	if s.End().Value != 3000 {
		t.Fatalf("End got %v", s.End())
	}
}
