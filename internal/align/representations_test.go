package align

import (
	"errors"
	"testing"

	"github.com/relayteam/relay/internal/media"
	"github.com/relayteam/relay/internal/segment"
	"github.com/relayteam/relay/internal/timeline"
)

func fragForTest(rendition string, idx, pts int64) segment.Fragment {
	rec := timeline.Record{
		Track: "main", Rendition: rendition, Seq: idx,
		PTS90k: pts, Dur90k: 3000, RecordNo: int(idx) + 1, LineNo: int(idx) + 1,
	}
	return segment.Fragment{
		Record: rec, SegmentIndex: int(pts / segment.DefaultSegmentTicks),
		CutStart90k: (pts / segment.DefaultSegmentTicks) * segment.DefaultSegmentTicks,
		CutEnd90k:   ((pts / segment.DefaultSegmentTicks) + 1) * segment.DefaultSegmentTicks,
	}
}

func TestAlignSharesExactBoundariesAcrossRepresentations(t *testing.T) {
	a := NewAligner(segment.DefaultSegmentTicks)
	for seg := int64(0); seg < 3; seg++ {
		for off := int64(0); off < 30; off++ {
			pts := seg*segment.DefaultSegmentTicks + off*3000
			if err := a.Add(fragForTest("1080p", seg*30+off, pts), 128); err != nil {
				t.Fatal(err)
			}
			if err := a.Add(fragForTest("720p", seg*30+off, pts), 64); err != nil {
				t.Fatal(err)
			}
		}
	}
	rep, err := a.Report()
	if err != nil {
		t.Fatal(err)
	}
	if rep.PrecisionTicks != 0 {
		t.Fatalf("precision=%d, want 0", rep.PrecisionTicks)
	}
	if rep.SegmentCount != 3 {
		t.Fatalf("segments=%d, want 3", rep.SegmentCount)
	}
	if rep.Duration90k != 3*segment.DefaultSegmentTicks {
		t.Fatalf("duration=%d", rep.Duration90k)
	}
}

func TestAlignAcceptsOneTickAndRejectsTwoTicks(t *testing.T) {
	a := NewAligner(segment.DefaultSegmentTicks)
	// One-tick endpoint drift remains within tolerance.
	f := fragForTest("720p", 0, 0)
	f.Record.Dur90k = segment.DefaultSegmentTicks + 1
	if err := a.Add(f, 10); err != nil {
		t.Fatalf("one tick must be allowed: %v", err)
	}
	if _, err := a.Report(); err != nil {
		t.Fatalf("report with one-tick drift: %v", err)
	}

	bad := fragForTest("1080p", 99, 0)
	bad.Record.RecordNo = 4242
	bad.Record.Dur90k = segment.DefaultSegmentTicks + 2
	var err error
	err = a.Add(bad, 10)
	if err != nil {
		t.Fatalf("end boundary is checked when report is finalized: %v", err)
	}
	_, err = a.Report()
	if err == nil {
		t.Fatal("two-tick drift must fail")
	}
	if !errors.Is(err, media.ErrBoundaryMismatch) {
		t.Fatalf("err=%v, want boundary mismatch", err)
	}
	me, ok := media.First(err)
	if !ok || me.Meta.RecordNo != 4242 {
		t.Fatalf("error location not preserved: %+v", err)
	}
}

func TestAlignRejectsCoverageMismatch(t *testing.T) {
	a := NewAligner(segment.DefaultSegmentTicks)
	for _, r := range []string{"1080p", "720p"} {
		for seg := int64(0); seg < 2; seg++ {
			pts := seg * segment.DefaultSegmentTicks
			f := fragForTest(r, seg*30, pts)
			f.Record.Dur90k = segment.DefaultSegmentTicks
			if err := a.Add(f, 1); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Add a third segment only to one representation.
	f := fragForTest("720p", 2, 2*segment.DefaultSegmentTicks)
	f.Record.Dur90k = segment.DefaultSegmentTicks
	if err := a.Add(f, 1); err != nil {
		t.Fatal(err)
	}
	_, err := a.Report()
	if !errors.Is(err, media.ErrDurationMismatch) {
		t.Fatalf("err=%v, want duration mismatch", err)
	}
}
