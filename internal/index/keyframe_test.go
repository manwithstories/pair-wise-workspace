package index

import (
	"testing"

	"mediatool/internal/clock"
	"mediatool/internal/fanout"
	"mediatool/internal/fixture"
	"mediatool/internal/repair"
	"mediatool/internal/ts"
)

func buildIndex(t *testing.T) *Index {
	t.Helper()
	s := fixture.Generate(7, 6000, fixture.FaultSpec{})
	res := ts.Parse(s.Data)
	tl := repair.Recover(res, fixture.PIDVideo, 2000, nil)
	reps := []fanout.Representation{
		{ID: 0, Bitrate: 800_000, KeyInterval: clock.MsToTicks(2000)},
		{ID: 1, Bitrate: 400_000, KeyInterval: clock.MsToTicks(1000)},
	}
	e := &fanout.Encoder{Dir: t.TempDir(), Seed: 42}
	_, keys, err := e.Encode(reps, tl)
	if err != nil {
		t.Fatal(err)
	}
	return New(keys)
}

func TestQuery(t *testing.T) {
	ix := buildIndex(t)
	if len(ix.Entries) == 0 {
		t.Fatal("empty index")
	}
	// 片段起点必命中关键帧。
	e, ok := ix.Query(0, clock.MsToTicks(2000))
	if !ok {
		t.Fatal("no keyframe at segment boundary")
	}
	if e.Time > clock.MsToTicks(2000) {
		t.Fatalf("returned keyframe after query time: %d", e.Time)
	}
	// 时刻之前没有关键帧时不命中。
	if _, ok := ix.Query(0, -1); ok {
		t.Fatal("query before first keyframe must miss")
	}
}

func TestHitRate(t *testing.T) {
	ix := buildIndex(t)
	hits, total, err := ix.HitRate()
	if err != nil {
		t.Fatal(err)
	}
	if total == 0 || hits != total {
		t.Fatalf("hits=%d total=%d", hits, total)
	}
}

func TestVerifyRejectsCorruptOffset(t *testing.T) {
	ix := buildIndex(t)
	e := ix.Entries[0]
	e.Offset += 8 // 人为破坏偏移
	ok, err := VerifyEntry(e)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("corrupt offset must not verify")
	}
}
