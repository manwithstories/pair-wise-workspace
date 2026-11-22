package model

import (
	"encoding/json"
	"testing"
)

func TestRangeOperations(t *testing.T) {
	r := Range{Start: 10, End: 20}
	if r.Empty() || r.Len() != 10 {
		t.Fatalf("range len=%d", r.Len())
	}
	if (Range{Start: 5, End: 5}).Empty() != true {
		t.Fatal("inverted range must be empty")
	}
	// 相交判定。
	if !r.Overlap(Range{Start: 15, End: 25}) {
		t.Fatal("ranges [10,20) and [15,25) should overlap")
	}
	if r.Overlap(Range{Start: 20, End: 30}) {
		t.Fatal("half-open ranges touching at boundary must not overlap")
	}
	// 合并取并集外包。
	m := r.Merge(Range{Start: 18, End: 40})
	if m.Start != 10 || m.End != 40 {
		t.Fatalf("merge result wrong: %+v", m)
	}
}

func TestEventBuilder(t *testing.T) {
	ev := NewEvent(CodeBurstLoss, SevWarning, Range{Start: 0, End: 10}, "burst").
		WithSeq(5, 8).
		WithDetail("lost", 4)
	if ev.SeqStart != 5 || ev.SeqEnd != 8 {
		t.Fatalf("seq range wrong: %+v", ev)
	}
	if ev.Detail["lost"] != 4 {
		t.Fatalf("detail missing: %+v", ev.Detail)
	}
	// 零值事件的序号缺省为 -1。
	plain := NewEvent(CodeTruncated, SevError, Range{}, "x")
	if plain.SeqStart != -1 || plain.SeqEnd != -1 {
		t.Fatalf("default seq should be -1: %+v", plain)
	}
}

func TestEventJSONShape(t *testing.T) {
	ev := NewEvent(CodeDupConflict, SevError, Range{Start: 1, End: 2}, "conflict").
		WithSeq(3, 3)
	raw, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"code", "severity", "time_range", "seq_start", "seq_end", "message"} {
		if _, ok := back[key]; !ok {
			t.Fatalf("JSON missing key %q in %s", key, raw)
		}
	}
	// 无 Detail 时该字段应被 omitempty 省略。
	if _, ok := back["detail"]; ok {
		t.Fatalf("detail should be omitted when nil: %s", raw)
	}
}

func TestCorruptRangeCarriesMillis(t *testing.T) {
	cr := CorruptRange{StartSample: 48000, EndSample: 96000, StartMs: 1000, EndMs: 2000}
	raw, _ := json.Marshal(cr)
	var back map[string]any
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back["start_ms"].(float64) != 1000 {
		t.Fatalf("start_ms round trip failed: %s", raw)
	}
}
