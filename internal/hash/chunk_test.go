package hash

import (
	"strings"
	"testing"

	"mediapack/internal/clock"
	"mediapack/internal/fault"
	"mediapack/internal/index"
)

func seg(ordinal int, frames ...Frame) Segment {
	start := clock.MustNew(int64(ordinal-1), 1)
	return Segment{Ordinal: ordinal, Start: start, End: clock.MustNew(int64(ordinal), 1), Frames: frames}
}

func frame(track string, sample int, payload string) Frame {
	return Frame{Track: track, Sample: sample, Payload: []byte(payload)}
}

func TestDigestIsStableAndOrderSensitive(t *testing.T) {
	a := seg(1, frame("v", 1, "alpha"), frame("v", 2, "beta"))
	b := seg(1, frame("v", 1, "alpha"), frame("v", 2, "beta"))
	ea, err := Digest(a, []string{"800k"})
	if err != nil {
		t.Fatal(err)
	}
	eb, err := Digest(b, []string{"800k"})
	if err != nil {
		t.Fatal(err)
	}
	if ea.SHA256 != eb.SHA256 || len(ea.SHA256) != 64 {
		t.Fatalf("digest not stable: %s %s", ea.SHA256, eb.SHA256)
	}
	reordered := seg(1, frame("v", 2, "beta"), frame("v", 1, "alpha"))
	ec, _ := Digest(reordered, []string{"800k"})
	if ec.SHA256 == ea.SHA256 {
		t.Fatal("digest does not depend on frame order")
	}
}

func TestDigestDependsOnBoundariesAndRepresentations(t *testing.T) {
	a := seg(1, frame("v", 1, "alpha"))
	base, _ := Digest(a, []string{"800k"})
	shifted := a
	shifted.Start = clock.MustNew(1, 90000)
	sh, _ := Digest(shifted, []string{"800k"})
	if sh.SHA256 == base.SHA256 {
		t.Fatal("digest ignores segment start")
	}
	other, _ := Digest(a, []string{"1600k"})
	if other.SHA256 == base.SHA256 {
		t.Fatal("digest ignores representation list")
	}
}

func TestDigestRejectsCorruptPayload(t *testing.T) {
	f := frame("v", 1, "alpha")
	f.SHA256 = strings.Repeat("0", 64)
	_, err := Digest(seg(1, f), []string{"800k"})
	fe, ok := fault.As(err)
	if !ok || fe.Code != "checksum-mismatch" || fe.Frame != 1 || fe.Segment != 1 {
		t.Fatalf("got %v", err)
	}
}

func TestDigestRejectsMissingPayloadAndEmptySegment(t *testing.T) {
	_, err := Digest(seg(1, Frame{Track: "v", Sample: 2}), []string{"800k"})
	if fe, ok := fault.As(err); !ok || fe.Code != "missing-payload" {
		t.Fatalf("got %v", err)
	}
	empty := Segment{Ordinal: 1, Start: clock.MustNew(1, 1), End: clock.MustNew(1, 1)}
	if _, err := Digest(empty, nil); fault.ClassOf(err) != fault.ClassInput {
		t.Fatalf("got %v", err)
	}
	if _, err := Digest(Segment{Start: clock.MustNew(0, 1), End: clock.MustNew(1, 1)}, nil); fault.ClassOf(err) != fault.ClassInternal {
		t.Fatalf("got %v", err)
	}
}

func TestBuildMarksBadSegmentBlockedAndKeepsGoodOnes(t *testing.T) {
	good := seg(1, frame("v", 1, "alpha"))
	bad := seg(2, frame("v", 3, "gamma"))
	bad.Frames[0].SHA256 = strings.Repeat("f", 64)
	m, err := Build([]Segment{bad, good}, []string{"800k"}, clock.MustNew(1, 1), DefaultLimits())
	fe, ok := fault.As(err)
	if !ok || fe.Code != "checksum-mismatch" {
		t.Fatalf("got %v", err)
	}
	if !m.Blocked {
		t.Fatal("manifest not marked blocked")
	}
	if m.Segments[0].Ordinal != 1 || m.Segments[0].SHA256 == "" || m.Segments[0].Blocked {
		t.Fatalf("good segment not hashed: %+v", m.Segments[0])
	}
	if !m.Segments[1].Blocked || m.Segments[1].SHA256 != "" {
		t.Fatalf("bad segment entered success list: %+v", m.Segments[1])
	}
}

func TestBuildRejectsDuplicatesAndLimits(t *testing.T) {
	one := seg(1, frame("v", 1, "alpha"))
	if _, err := Build([]Segment{one, one}, nil, clock.MustNew(1, 1), DefaultLimits()); fault.ClassOf(err) != fault.ClassInput {
		t.Fatalf("duplicate: %v", err)
	}
	if _, err := Build([]Segment{one, seg(2, frame("v", 2, "b"))}, nil, clock.MustNew(1, 1), Limits{MaxSegments: 1}); fault.ClassOf(err) != fault.ClassLimit {
		t.Fatalf("limit: %v", err)
	}
}

func TestAllMatchDetectsDivergence(t *testing.T) {
	a, _ := Build([]Segment{seg(1, frame("v", 1, "alpha"))}, []string{"800k"}, clock.MustNew(1, 1), DefaultLimits())
	b, _ := Build([]Segment{seg(1, frame("v", 1, "alpha"))}, []string{"800k"}, clock.MustNew(1, 1), DefaultLimits())
	if !AllMatch(a, b) {
		t.Fatal("identical manifests differ")
	}
	c, _ := Build([]Segment{seg(1, frame("v", 1, "changed"))}, []string{"800k"}, clock.MustNew(1, 1), DefaultLimits())
	if AllMatch(a, c) {
		t.Fatal("divergent manifests compared equal")
	}
	if AllMatch(a, nil) || AllMatch(nil, nil) {
		t.Fatal("nil manifests compared equal")
	}
}

func TestFromIndexSelectsRange(t *testing.T) {
	tab := &index.Table{Starts: []clock.Ticks{
		clock.MustNew(0, 1), clock.MustNew(1, 1), clock.MustNew(2, 1), clock.MustNew(3, 1),
	}}
	payloads := map[int][]byte{1: []byte("a"), 2: []byte("b"), 3: []byte("c"), 4: []byte("d")}
	frames, err := FromIndex("v", tab, clock.MustNew(1, 1), clock.MustNew(3, 1), payloads)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 2 || frames[0].Sample != 2 || frames[1].Sample != 3 {
		t.Fatalf("frames=%+v", frames)
	}
	if _, err := FromIndex("v", tab, clock.MustNew(9, 1), clock.MustNew(10, 1), payloads); fault.ClassOf(err) != fault.ClassInput {
		t.Fatalf("empty range: %v", err)
	}
}
