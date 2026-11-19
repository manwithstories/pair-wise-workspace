package subtitle

import (
	"strings"
	"testing"

	"review/internal/issues"
)

func TestSRTValid(t *testing.T) {
	data := []byte("1\n00:00:00,500 --> 00:00:02,000\nhello\n\n2\n00:00:02,500 --> 00:00:04,000\nworld\n\n")
	cues, iss := Parse(data, "t.srt", "t.srt")
	if len(iss) != 0 {
		t.Fatalf("unexpected issues: %v", iss)
	}
	if len(cues) != 2 {
		t.Fatalf("got %d cues", len(cues))
	}
	if cues[0].ByteOffset != 0 || cues[1].ByteOffset != int64(strings.Index(string(data), "2\n")) {
		t.Fatalf("bad offsets: %d %d", cues[0].ByteOffset, cues[1].ByteOffset)
	}
	if cues[0].Start != 0.5 || cues[0].End != 2.0 {
		t.Fatalf("bad times: %v %v", cues[0].Start, cues[0].End)
	}
	if cues[1].Text != "world" {
		t.Fatalf("bad text %q", cues[1].Text)
	}
}

func TestSRTOverlapInvalidTruncated(t *testing.T) {
	data := []byte("1\n00:00:00,000 --> 00:00:02,000\na\n\n" +
		"2\n00:00:01,000 --> 00:00:03,000\noverlap\n\n" +
		"3\n00:00:99,000 --> 00:00:98,000\nbad\n\n" +
		"4\n00:00:05,000 --> 00:00:06,000\nok\n\n" +
		"5\n00:00:07,0") // cut mid-line
	cues, iss := Parse(data, "t.srt", "t.srt")
	if len(cues) != 2 { // cue 1 and cue 4 survive
		t.Fatalf("got %d cues, want 2", len(cues))
	}
	kinds := map[string]int{}
	for _, is := range iss {
		kinds[is.Kind]++
		if is.ByteOffset < 0 || is.Raw == "" {
			t.Fatalf("issue not located/raw: %+v", is)
		}
	}
	if kinds[issues.Overlap] != 1 || kinds[issues.InvalidTime] != 1 || kinds[issues.TruncatedRecord] != 1 {
		t.Fatalf("kinds: %v", kinds)
	}
}

func TestVTT(t *testing.T) {
	data := []byte("WEBVTT\n\nNOTE ignored\n\ncue-1\n00:00:01.000 --> 00:00:02.500\nhi\n\n00:00:04.000 --> 00:00:05.000 align:start\nbye\n\n")
	cues, iss := Parse(data, "t.vtt", "t.vtt")
	if len(iss) != 0 {
		t.Fatalf("unexpected issues: %v", iss)
	}
	if len(cues) != 2 {
		t.Fatalf("got %d cues", len(cues))
	}
	if cues[0].Start != 1.0 || cues[1].End != 5.0 {
		t.Fatalf("bad times: %+v", cues)
	}
}

func TestVTTTruncated(t *testing.T) {
	data := []byte("WEBVTT\n\n00:00:01.000 --> 00:00:02.000\nok\n\n00:00:03.0")
	_, iss := Parse(data, "t.vtt", "t.vtt")
	if len(iss) != 1 || iss[0].Kind != issues.TruncatedRecord {
		t.Fatalf("issues: %v", iss)
	}
}
