package subtitle_test

import (
	"strings"
	"testing"

	"mediasync/internal/media"
	"mediasync/internal/subtitle"
	"mediasync/internal/testdata"
)

func findIssue(res *subtitle.Result, code media.Code) *media.Issue {
	for _, is := range res.Issues {
		if is.Code == code {
			return is
		}
	}
	return nil
}

func TestParseSRTClean(t *testing.T) {
	res := subtitle.ParseSRT(testdata.GenerateSRT(testdata.SubtitleClean, 42), "cam-a.srt")
	if len(res.Issues) != 0 {
		t.Fatalf("unexpected issues: %v", res.Issues)
	}
	if len(res.Cues) != 5 {
		t.Fatalf("got %d cues, want 5", len(res.Cues))
	}
	first := res.Cues[0]
	if first.StartNS != 1_000_000_000 || first.EndNS != 2_500_000_000 {
		t.Errorf("cue 0 timing = [%d,%d)", first.StartNS, first.EndNS)
	}
	if first.Index != 1 || first.StartLine != 2 {
		t.Errorf("cue 0 index/line = %d/%d", first.Index, first.StartLine)
	}
	if got := res.Cues[2].Content(); got != "乐队起奏 第一乐章" {
		t.Errorf("markup not stripped: %q", got)
	}
	// Offsets must point at the raw index digit of each record.
	data := testdata.GenerateSRT(testdata.SubtitleClean, 42)
	for i, cue := range res.Cues {
		want := strings.Index(string(data), "\n"+itoa(i+1)+"\n")
		if want >= 0 {
			want++
			if cue.Offset != int64(want) {
				t.Errorf("cue %d offset = %d, want %d", i, cue.Offset, want)
			}
		}
	}
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var b []byte
	for v > 0 {
		b = append([]byte{byte('0' + v%10)}, b...)
		v /= 10
	}
	return string(b)
}

func TestParseSRTRecoversFromBadTimestamps(t *testing.T) {
	data := testdata.GenerateSRT(testdata.SubtitleBadTime, 42)
	res := subtitle.ParseSRT(data, "cam-c.srt")
	if len(res.Cues) != 3 {
		t.Fatalf("got %d cues, want 3 recoverable cues", len(res.Cues))
	}
	bad := findIssue(res, media.CodeInvalidTimestamp)
	if bad == nil {
		t.Fatal("no invalid_timestamp issue")
	}
	if !bad.HasOffset || bad.Offset <= 0 {
		t.Errorf("invalid timestamp has no raw offset: %+v", bad)
	}
	line := strings.Count(string(data[:bad.Offset]), "\n") + 1
	if line != 14 {
		t.Errorf("invalid timestamp reported at line %d, want 14", line)
	}
	if !strings.HasPrefix(string(data[bad.Offset:]), "00:00:06,0x0") {
		t.Errorf("offset %d does not point at the bad token: %q", bad.Offset, data[bad.Offset:])
	}
	mal := findIssue(res, media.CodeMalformed)
	if mal == nil {
		t.Fatal("no malformed issue for the '->' arrow")
	}
	if line := strings.Count(string(data[:mal.Offset]), "\n") + 1; line != 10 {
		t.Errorf("malformed arrow reported at line %d, want 10", line)
	}
	// The record after the damage is still parsed.
	if last := res.Cues[len(res.Cues)-1]; last.Text != "字幕组测试文本" {
		t.Errorf("tail cue not recovered: %q", last.Text)
	}
}

func TestParseSRTOverlap(t *testing.T) {
	res := subtitle.ParseSRT(testdata.GenerateSRT(testdata.SubtitleOverlap, 42), "cam-o.srt")
	if len(res.Cues) != 5 {
		t.Fatalf("got %d cues, want 5", len(res.Cues))
	}
	is := findIssue(res, media.CodeOverlap)
	if is == nil {
		t.Fatal("overlapping cue not reported")
	}
	if is.Offset == 0 {
		t.Error("overlap issue has no offset")
	}
	overlapped := 0
	for _, c := range res.Cues {
		if c.Overlap {
			overlapped++
		}
	}
	if overlapped != 1 {
		t.Errorf("%d cues flagged overlapping, want 1", overlapped)
	}
}

func TestParseSRTTruncated(t *testing.T) {
	res := subtitle.ParseSRT(testdata.GenerateSRT(testdata.SubtitleTruncated, 42), "cam-t.srt")
	is := findIssue(res, media.CodeTruncated)
	if is == nil {
		t.Fatal("truncated record not reported")
	}
	if !is.HasOffset || is.Offset <= 0 {
		t.Errorf("truncated record lacks offset: %+v", is)
	}
	if len(res.Cues) != 4 {
		t.Errorf("got %d recoverable cues, want 4", len(res.Cues))
	}
}

func TestParseWebVTT(t *testing.T) {
	res := subtitle.ParseWebVTT(testdata.GenerateSRT(testdata.SubtitleWebVTT, 42), "cam-b.vtt")
	if len(res.Issues) != 0 {
		t.Fatalf("unexpected issues: %v", res.Issues)
	}
	if len(res.Cues) != 5 {
		t.Fatalf("got %d cues, want 5", len(res.Cues))
	}
	if res.Format != "webvtt" {
		t.Errorf("format = %q", res.Format)
	}
	// Cue settings after the timestamp must not affect parsing.
	if res.Cues[0].StartNS != 1_000_000_000 {
		t.Errorf("cue 0 start = %d", res.Cues[0].StartNS)
	}
}

func TestParseWebVTTBadTimestamp(t *testing.T) {
	res := subtitle.ParseWebVTT(testdata.GenerateSRT(testdata.SubtitleVTTBadTime, 42), "cam-b.vtt")
	is := findIssue(res, media.CodeInvalidTimestamp)
	if is == nil {
		t.Fatal("comma decimal in WebVTT not reported")
	}
	if is.Offset == 0 {
		t.Error("VTT issue lacks offset")
	}
	if len(res.Cues) != 4 {
		t.Errorf("got %d cues, want 4", len(res.Cues))
	}
}

func TestParseWebVTTMissingSignature(t *testing.T) {
	res := subtitle.ParseWebVTT([]byte("00:00:01.000 --> 00:00:02.000\nhi\n"), "x.vtt")
	if !res.Fatal() {
		t.Fatal("missing WEBVTT signature must be fatal")
	}
}

func TestDetect(t *testing.T) {
	if got := subtitle.Detect(testdata.GenerateSRT(testdata.SubtitleClean, 42), "a.srt"); got.Format != "srt" {
		t.Errorf("detect srt = %q", got.Format)
	}
	if got := subtitle.Detect(testdata.GenerateSRT(testdata.SubtitleWebVTT, 42), "b"); got.Format != "webvtt" {
		t.Errorf("detect by content = %q", got.Format)
	}
}

func TestSubtitleSizeLimit(t *testing.T) {
	big := make([]byte, subtitle.MaxSubtitleBytes+1)
	res := subtitle.ParseSRT(big, "big.srt")
	if !res.Fatal() {
		t.Fatal("oversized input must be fatal")
	}
}

func TestParseCRLF(t *testing.T) {
	data := []byte("1\r\n00:00:01,000 --> 00:00:02,000\r\nhello\r\n\r\n")
	res := subtitle.ParseSRT(data, "crlf.srt")
	if len(res.Cues) != 1 {
		t.Fatalf("got %d cues: %v", len(res.Cues), res.Issues)
	}
	if res.Cues[0].Text != "hello" {
		t.Errorf("text = %q", res.Cues[0].Text)
	}
}
