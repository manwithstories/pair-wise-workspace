package fault

import (
	"errors"
	"fmt"
	"testing"

	"bcast/internal/model"
)

func TestErrorWrapsSentinels(t *testing.T) {
	e := Boundaryf("half-open interval gap at %d", 42)
	if !errors.Is(e, ErrBoundary) {
		t.Fatal("errors.Is must reach ErrBoundary")
	}
	in := InvalidInputf("bad seed %d", -1)
	if !errors.Is(in, ErrInvalidInput) {
		t.Fatal("errors.Is must reach ErrInvalidInput")
	}
	corrupt := New(ErrChecksum, KindChecksum, 2, 7, 100, 148, "crc mismatch")
	if !errors.Is(corrupt, ErrChecksum) {
		t.Fatal("errors.Is must reach ErrChecksum")
	}
	if s := corrupt.Error(); s == "" || fmt.Sprintf("%v", corrupt) == "" {
		t.Fatal("error string must be non-empty")
	}
}

func TestCollectorOrderingAndCounts(t *testing.T) {
	var c Collector
	c.Record(KindDrop, 0, 3, 0, 0, "missing seq 3")
	c.Record(KindDrop, 0, 4, 0, 0, "missing seq 4")
	c.Record(KindDupe, 1, 9, 64, 128, "duplicate")
	got := c.Items()
	want := []model.Fault{
		{Kind: KindDrop, Seq: 3, Detail: "missing seq 3"},
		{Kind: KindDrop, Seq: 4, Detail: "missing seq 4"},
		{Kind: KindDupe, TrackID: 1, Seq: 9, ByteStart: 64, ByteEnd: 128, Detail: "duplicate"},
	}
	if len(got) != len(want) || c.Count() != 3 {
		t.Fatalf("count = %d", c.Count())
	}
	for i := range want {
		if got[i].Kind != want[i].Kind || got[i].Seq != want[i].Seq ||
			got[i].Detail != want[i].Detail || got[i].ByteStart != want[i].ByteStart {
			t.Fatalf("item %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if c.CountByKind(KindDrop) != 2 || c.CountByKind(KindChecksum) != 0 {
		t.Fatal("CountByKind wrong")
	}
	// 返回副本，外部修改不污染收集器。
	got[0].Kind = "tampered"
	if c.Items()[0].Kind != KindDrop {
		t.Fatal("Items() must return a defensive copy")
	}
}
