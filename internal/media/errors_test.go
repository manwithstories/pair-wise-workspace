package media_test

import (
	"encoding/json"
	"strings"
	"testing"

	"mediasync/internal/media"
)

func TestIssueErrorIncludesContext(t *testing.T) {
	is := media.New(media.StageSubtitle, media.InputSRT, media.CodeInvalidTimestamp,
		"bad start timestamp %q", "00:00:0x").
		WithFile("cam-a.srt").
		WithOffset(177).
		WithRecord(3)
	got := is.Error()
	for _, want := range []string{"cam-a.srt", "@177", "subtitle", "srt", "invalid_timestamp", "00:00:0x"} {
		if !strings.Contains(got, want) {
			t.Errorf("error %q missing %q", got, want)
		}
	}
}

func TestIssueSeverity(t *testing.T) {
	rec := media.New(media.StageMP4, media.InputMP4, media.CodeOutOfRange, "x").WithOffset(10)
	if !rec.Recoverable() || rec.Fatal {
		t.Error("default issue must be recoverable")
	}
	fatal := media.New(media.StageMP4, media.InputMP4, media.CodeMalformed, "y").AsFatal()
	if fatal.Recoverable() || !fatal.Fatal {
		t.Error("AsFatal did not mark the issue fatal")
	}
	if media.FirstFatal([]*media.Issue{rec, fatal}) != fatal {
		t.Error("FirstFatal did not find the fatal issue")
	}
	if media.FirstFatal([]*media.Issue{rec}) != nil {
		t.Error("FirstFatal returned a non-fatal issue")
	}
}

func TestCountingAndFiltering(t *testing.T) {
	withOffset := media.New(media.StageRTP, media.InputRTP, media.CodePacketLoss, "lost").WithOffset(1)
	noOffset := media.New(media.StageRTP, media.InputRTP, media.CodePacketLoss, "lost")
	withSeq := media.New(media.StageRTP, media.InputRTP, media.CodeDuplicatePacket, "dup").WithSeq(9)
	issues := []*media.Issue{withOffset, noOffset, withSeq}
	if got := media.CountWithOffset(issues); got != 1 {
		t.Errorf("CountWithOffset = %d, want 1", got)
	}
	filtered := media.Filter(issues, func(is *media.Issue) bool { return is.HasSeq })
	if len(filtered) != 1 || filtered[0] != withSeq {
		t.Errorf("filtered = %v", filtered)
	}
	if got := len(media.Filter(issues, func(*media.Issue) bool { return true })); got != 3 {
		t.Errorf("filter-all = %d, want 3", got)
	}
}

func TestJSONShape(t *testing.T) {
	is := media.New(media.StageMP4, media.InputMP4, media.CodeOutOfRange, "sample oob").
		WithOffset(13249).WithRecord(9)
	data, err := json.Marshal(is)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"stage", "input", "code", "message", "offset", "has_offset", "fatal"} {
		if _, ok := decoded[key]; !ok {
			t.Errorf("JSON missing %q: %s", key, data)
		}
	}
	if decoded["has_offset"] != true {
		t.Errorf("has_offset = %v", decoded["has_offset"])
	}
}

func TestSortStable(t *testing.T) {
	a := media.New(media.StageMP4, media.InputMP4, media.CodeMalformed, "a").WithFile("b.mp4").WithOffset(10)
	b := media.New(media.StageMP4, media.InputMP4, media.CodeMalformed, "b").WithFile("a.mp4").WithOffset(50)
	c := media.New(media.StageMP4, media.InputMP4, media.CodeMalformed, "c").WithFile("a.mp4").WithOffset(5)
	d := media.New(media.StageMP4, media.InputMP4, media.CodeMalformed, "d").WithFile("a.mp4")
	issues := []*media.Issue{a, b, c, d}
	media.SortStable(issues)
	want := []string{"d", "c", "b", "a"}
	for i, is := range issues {
		if is.Message != want[i] {
			t.Fatalf("position %d = %q, want %q", i, is.Message, want[i])
		}
	}
}
