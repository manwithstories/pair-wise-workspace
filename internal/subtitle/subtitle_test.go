package subtitle

import (
	"strings"
	"testing"
)

const sampleSRT = `1
00:00:01,000 --> 00:00:03,500
棚内访谈：机位 A

2
00:00:04,250 --> 00:00:06,000
外景机位 B
第二行文本
`

func TestParseSRTRoundtrip(t *testing.T) {
	doc, err := Parse(FormatSRT, []byte(sampleSRT), Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Cues) != 2 {
		t.Fatalf("cues = %d, want 2", len(doc.Cues))
	}
	if !doc.RoundtripEqual {
		t.Errorf("round trip not equal; rendered:\n%s", Serialize(doc))
	}
	if len(doc.Issues) != 0 {
		t.Errorf("unexpected issues: %v", doc.Issues)
	}
	c := doc.Cues[0]
	if c.StartNs != 1_000_000_000 || c.EndNs != 3_500_000_000 {
		t.Errorf("cue0 times = %d..%d", c.StartNs, c.EndNs)
	}
	if c.Text != "棚内访谈：机位 A" {
		t.Errorf("cue0 text = %q", c.Text)
	}
	if doc.Cues[1].Text != "外景机位 B\n第二行文本" {
		t.Errorf("cue1 text = %q", doc.Cues[1].Text)
	}
	if !strings.Contains(Serialize(doc), "00:00:04,250 --> 00:00:06,000") {
		t.Errorf("serialization lost original precision:\n%s", Serialize(doc))
	}
}

func TestParseSRTRawPrecisionPreserved(t *testing.T) {
	// Sub-millisecond source precision must survive round trip.
	src := "1\n00:00:01,000100 --> 00:00:02,500900\nx\n"
	doc, err := Parse(FormatSRT, []byte(src), Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if doc.Cues[0].StartRaw != "00:00:01,000100" {
		t.Fatalf("raw start = %q", doc.Cues[0].StartRaw)
	}
	if doc.Cues[0].StartNs != 1_000_100_000 {
		t.Fatalf("start ns = %d", doc.Cues[0].StartNs)
	}
	if !doc.RoundtripEqual {
		t.Fatalf("sub-ms precision not preserved:\n%s", Serialize(doc))
	}
}

func TestParseSRTDefects(t *testing.T) {
	t.Run("invalid index", func(t *testing.T) {
		src := "1\n00:00:01,000 --> 00:00:02,000\na\n\n5\n00:00:03,000 --> 00:00:04,000\nb\n"
		doc, err := Parse(FormatSRT, []byte(src), Limits{})
		if err != nil {
			t.Fatal(err)
		}
		if len(doc.Cues) != 2 {
			t.Fatalf("cues = %d", len(doc.Cues))
		}
		found := false
		for _, is := range doc.Issues {
			if is.Class == "subtitle.invalid-index" {
				found = true
				if is.Locator.CueIndex != 2 {
					t.Errorf("issue locator cue = %d, want 2", is.Locator.CueIndex)
				}
			}
		}
		if !found {
			t.Errorf("no invalid-index issue: %v", doc.Issues)
		}
	})
	t.Run("negative duration", func(t *testing.T) {
		src := "1\n00:00:05,000 --> 00:00:02,000\nreversed\n"
		doc, err := Parse(FormatSRT, []byte(src), Limits{})
		if err != nil {
			t.Fatal(err)
		}
		if doc.Cues[0].EndNs < doc.Cues[0].StartNs {
			t.Error("negative span not normalized")
		}
		found := false
		for _, is := range doc.Issues {
			if is.Class == "subtitle.negative-duration" {
				found = true
			}
		}
		if !found {
			t.Errorf("no negative-duration issue: %v", doc.Issues)
		}
	})
	t.Run("truncated tail cue", func(t *testing.T) {
		// Timing line at EOF, no text, no final newline.
		src := "1\n00:00:01,000 --> 00:00:02,000\n"
		doc, err := Parse(FormatSRT, []byte(src), Limits{})
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, is := range doc.Issues {
			if is.Class == "subtitle.truncated-cue" {
				found = true
			}
		}
		if !found {
			t.Errorf("no truncated-cue issue: %v; cues=%d", doc.Issues, len(doc.Cues))
		}
	})
	t.Run("malformed timestamp", func(t *testing.T) {
		src := "1\n00:00:0X,000 --> 00:00:02,000\na\n"
		doc, err := Parse(FormatSRT, []byte(src), Limits{})
		if err != nil {
			t.Fatal(err)
		}
		if len(doc.Cues) != 0 {
			t.Errorf("malformed cue accepted: %+v", doc.Cues)
		}
		if len(doc.Issues) == 0 {
			t.Error("no issue for malformed timestamp")
		}
	})
}

func TestParseSRTLimits(t *testing.T) {
	var b strings.Builder
	for i := 1; i <= 5; i++ {
		b.WriteString(itoa(i))
		b.WriteString("\n00:00:01,000 --> 00:00:02,000\nt\n\n")
	}
	doc, err := Parse(FormatSRT, []byte(b.String()), Limits{MaxCues: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Cues) != 3 {
		t.Errorf("cues = %d, want capped at 3", len(doc.Cues))
	}
	if !doc.Issues.Fatal() {
		t.Error("exceeding max cues should be a fatal issue")
	}
	if _, err := Parse(FormatSRT, make([]byte, 128), Limits{MaxBytes: 16}); err == nil {
		t.Error("oversized input accepted")
	}
}

const sampleVTT = `WEBVTT
Kind: captions

cue-1
00:00:01.000 --> 00:00:03.500 align:start position:10%
棚内访谈

00:04.250 --> 00:06.000
外景机位
`

func TestParseVTTRoundtrip(t *testing.T) {
	doc, err := Parse(FormatVTT, []byte(sampleVTT), Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Cues) != 2 {
		t.Fatalf("cues = %d, want 2", len(doc.Cues))
	}
	if doc.Cues[0].Identifier != "cue-1" {
		t.Errorf("identifier = %q", doc.Cues[0].Identifier)
	}
	if doc.Cues[0].Settings != "align:start position:10%" {
		t.Errorf("settings = %q", doc.Cues[0].Settings)
	}
	// MM:SS.mmm form is valid VTT.
	if doc.Cues[1].StartNs != 4_250_000_000 {
		t.Errorf("cue1 start = %d", doc.Cues[1].StartNs)
	}
	if !doc.RoundtripEqual {
		t.Errorf("VTT round trip mismatch:\n%s", Serialize(doc))
	}
	if len(doc.Issues) != 0 {
		t.Errorf("unexpected issues: %v", doc.Issues)
	}
}

func TestParseVTTMissingHeader(t *testing.T) {
	doc, err := Parse(FormatVTT, []byte("00:00:01.000 --> 00:00:02.000\nx\n"), Limits{})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, is := range doc.Issues {
		if is.Class == "subtitle.malformed-timestamp" && strings.Contains(is.Message, "WEBVTT") {
			found = true
		}
	}
	if !found {
		t.Errorf("missing header not reported: %v", doc.Issues)
	}
}

func TestCRLFNormalization(t *testing.T) {
	src := "1\r\n00:00:01,000 --> 00:00:02,000\r\na\r\nb\r\n"
	doc, err := Parse(FormatSRT, []byte(src), Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if doc.Cues[0].Text != "a\nb" {
		t.Errorf("CRLF text = %q", doc.Cues[0].Text)
	}
}

func TestParseTimestampEdgeCases(t *testing.T) {
	ok := map[string]int64{
		"00:00:01,500":       1_500_000_000,
		"00:00:01.5":         1_500_000_000,
		"1:02:03.004":        3_723_004_000_000,
		"00:00:00,000000001": 1,
	}
	for s, want := range ok {
		got, valid := ParseTimestamp(s)
		if !valid || got != want {
			t.Errorf("ParseTimestamp(%q) = %d,%v want %d", s, got, valid, want)
		}
	}
	bad := []string{"", "aa:bb", "00:61:00,000", "00:00:00,xyz", "00:00:00:00,"}
	for _, s := range bad {
		if _, valid := ParseTimestamp(s); valid {
			t.Errorf("ParseTimestamp(%q) accepted invalid input", s)
		}
	}
}
