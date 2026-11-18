package main

import (
	"strings"
	"testing"
)

func rec(track string, epoch, seq, pts int64, bits int) *Record {
	return &Record{
		Line: 1, Track: track, Epoch: epoch, Seq: seq, PTS: pts,
		TB: Timebase{1, 1_000_000}, Bits: bits,
	}
}

func TestUnwrap32Wrap(t *testing.T) {
	u := NewUnwrapper()
	const period int64 = 1 << 32
	// 末值 → 回绕后小值，真实增量 1000 应被还原。
	seq := []int64{period - 500, 500}
	var got []int64
	for i, pts := range seq {
		v, err := u.Unwrap(rec("video", 0, int64(i), pts, 32))
		if err != nil {
			t.Fatalf("wrap at %d: %v", i, err)
		}
		got = append(got, v)
	}
	if got[0] != 0 {
		t.Fatalf("first must zero: got %d", got[0])
	}
	// 归零口径：首点归零后，第二点 = 1000。
	if got[1] != 1000 {
		t.Fatalf("wrap unwrap: got %d want 1000", got[1])
	}
}

func TestUnwrap33BitPeriod(t *testing.T) {
	u := NewUnwrapper()
	const period int64 = 1 << 33
	pts := []int64{period - 100, 50}
	var last int64
	for i, p := range pts {
		v, err := u.Unwrap(rec("audio", 0, int64(i), p, 33))
		if err != nil {
			t.Fatalf("33bit %d: %v", i, err)
		}
		last = v
	}
	if last != 150 {
		t.Fatalf("33bit wrap: got %d want 150", last)
	}
}

func TestUnwrapEpochRestart(t *testing.T) {
	u := NewUnwrapper()
	// epoch 0 累积一段后切换 epoch 1，新 epoch 必须独立重新归零，
	// 即使原始计数器从小值重新开始也不得报错。
	v1, err := u.Unwrap(rec("video", 0, 0, 1_000_000, 32))
	if err != nil || v1 != 0 {
		t.Fatalf("e0 first: v=%d err=%v", v1, err)
	}
	if _, err := u.Unwrap(rec("video", 0, 1, 2_000_000, 32)); err != nil {
		t.Fatalf("e0 second: %v", err)
	}
	v3, err := u.Unwrap(rec("video", 1, 0, 123, 32))
	if err != nil {
		t.Fatalf("epoch restart must not error: %v", err)
	}
	if v3 != 0 {
		t.Fatalf("new epoch must re-zero: got %d", v3)
	}
	if len(u.Keys()) != 2 {
		t.Fatalf("want 2 segments, got %d", len(u.Keys()))
	}
}

func TestUnwrapRejectsNonWrapBackward(t *testing.T) {
	u := NewUnwrapper()
	// 倒退量超过半周期：负跳变后候选增量仍 >= 半周期，必须报错。
	if _, err := u.Unwrap(rec("video", 0, 0, 1000, 32)); err != nil {
		t.Fatal(err)
	}
	_, err := u.Unwrap(rec("video", 0, 1, 200, 32))
	if err == nil {
		t.Fatal("backward jump beyond half period must be rejected")
	}
	if !strings.Contains(err.Error(), "line 1") {
		t.Fatalf("error must carry line number: %v", err)
	}
}

func TestUnwrapRejectsHalfPeriodBoundary(t *testing.T) {
	u := NewUnwrapper()
	const half int64 = 1 << 31
	if _, err := u.Unwrap(rec("video", 0, 0, 0, 32)); err != nil {
		t.Fatal(err)
	}
	// 恰好半周期的正向跳变：规格要求“严格小于半周期”，边界应拒绝。
	_, err := u.Unwrap(rec("video", 0, 1, half, 32))
	if err == nil {
		t.Fatal("jump equal to half period must be rejected")
	}
}

func TestUnwrapDuplicateTimestamps(t *testing.T) {
	u := NewUnwrapper()
	// 量化后相同时间戳（零增量）合法，解绕值保持不变。
	v1, _ := u.Unwrap(rec("audio", 0, 0, 5000, 32))
	v2, err := u.Unwrap(rec("audio", 0, 1, 5000, 32))
	if err != nil {
		t.Fatalf("duplicate pts should be allowed: %v", err)
	}
	if v1 != 0 || v2 != 0 {
		t.Fatalf("duplicate pts unwrap: %d,%d want 0,0", v1, v2)
	}
}

func TestUnwrapRejectsTimebaseChange(t *testing.T) {
	u := NewUnwrapper()
	r1 := rec("video", 0, 0, 100, 32)
	if _, err := u.Unwrap(r1); err != nil {
		t.Fatal(err)
	}
	r2 := rec("video", 0, 1, 200, 32)
	r2.TB = Timebase{1, 90000}
	if _, err := u.Unwrap(r2); err == nil {
		t.Fatal("timebase change within an epoch must be rejected")
	}
}
