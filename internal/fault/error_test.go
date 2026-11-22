package fault

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestErrorSerialization(t *testing.T) {
	e := New(CatTiming, "non-monotonic event").
		WithTrack("video-720p").WithSeq(42).WithRawTime(1 << 40).WithOffset(128)
	data, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	var back Error
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if back.Category != CatTiming || back.Track != "video-720p" ||
		back.Seq != 42 || !back.HasSeq || back.RawTime != 1<<40 || back.Offset != 128 {
		t.Fatalf("roundtrip mismatch: %+v", back)
	}
	s := e.Error()
	for _, want := range []string{"timing", "video-720p", "seq=42", "offset=128"} {
		if !strings.Contains(s, want) {
			t.Fatalf("Error() %q missing %q", s, want)
		}
	}
}

func TestListCountAndJSON(t *testing.T) {
	var l List
	l.Add(New(CatParse, "truncated box"))
	l.Add(New(CatParse, "illegal length"))
	l.Add(New(CatLoss, "gap"))
	l.Add(nil)
	if l.Count(CatParse) != 2 || l.Count(CatLoss) != 1 || l.Count(CatAlign) != 0 {
		t.Fatalf("counts wrong: %+v", l.Errors)
	}
	if _, err := l.JSON(); err != nil {
		t.Fatal(err)
	}
}
