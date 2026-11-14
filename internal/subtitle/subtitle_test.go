package subtitle

import (
	"strings"
	"testing"
)

func TestSRTNormalizeAndRoundtrip(t *testing.T) {
	source := "1\n00:00:01,250 --> 00:00:03,750\nfirst\n\n" +
		"2\n00:00:02,000 --> 00:00:04,000\nsecond\n\n" +
		"3\n00:00:10,000 --> 00:00:10,000\nzero\n"
	track, err := ParseSRT([]byte(source))
	if err != nil {
		t.Fatal(err)
	}
	if len(track.Cues) != 3 || OverlapGroupCount(track.Cues) != 1 {
		t.Fatalf("cues=%d overlap_groups=%d", len(track.Cues), OverlapGroupCount(track.Cues))
	}
	if track.Cues[0].OverlapGroup != 1 || track.Cues[1].OverlapGroup != 1 {
		t.Fatalf("overlap groups were not assigned: %#v", track.Cues[:2])
	}
	if !track.Cues[2].Invalid || track.Cues[2].Reason != "invalid_cue" {
		t.Fatalf("zero-length cue not invalid: %#v", track.Cues[2])
	}
	data, err := Write(track, FormatSRT)
	if err != nil {
		t.Fatal(err)
	}
	roundtrip, err := ParseSRT(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(roundtrip.Cues) != 2 {
		t.Fatalf("eligible cues=%d, want 2", len(roundtrip.Cues))
	}
	for i := range roundtrip.Cues {
		if roundtrip.Cues[i].StartMS != track.Cues[i].StartMS ||
			roundtrip.Cues[i].EndMS != track.Cues[i].EndMS ||
			roundtrip.Cues[i].Text != track.Cues[i].Text {
			t.Fatalf("cue %d drifted: %#v != %#v", i, roundtrip.Cues[i], track.Cues[i])
		}
	}
}

func TestASSParseAndRoundtrip(t *testing.T) {
	source := "[Script Info]\nScriptType: v4.00+\n\n[Events]\n" +
		"Format: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text\n" +
		"Dialogue: 0,0:00:01.25,0:00:03.75,Default,Alice,0,0,0,,hello, world\\N第二行\n"
	track, err := ParseASS([]byte(source))
	if err != nil {
		t.Fatal(err)
	}
	cue := track.Cues[0]
	if cue.StartMS != 1250 || cue.EndMS != 3750 || cue.Style != "Default" || cue.Name != "Alice" {
		t.Fatalf("unexpected cue: %#v", cue)
	}
	if cue.Text != "hello, world\n第二行" {
		t.Fatalf("ASS text was not normalized: %q", cue.Text)
	}
	data, err := Write(track, FormatASS)
	if err != nil {
		t.Fatal(err)
	}
	roundtrip, err := ParseASS(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(roundtrip.Cues) != 1 || roundtrip.Cues[0].Text != cue.Text {
		t.Fatalf("ASS roundtrip mismatch: %#v", roundtrip.Cues)
	}
}

func TestVTTRoundtripIsExact(t *testing.T) {
	source := "WEBVTT\n\nintro\n00:01.250 --> 00:03.750\nhello\n"
	track, err := ParseVTT([]byte(source))
	if err != nil {
		t.Fatal(err)
	}
	if len(track.Cues) != 1 || track.Cues[0].StartMS != 1250 || track.Cues[0].ID != "intro" {
		t.Fatalf("unexpected VTT cue: %#v", track.Cues)
	}
	data, err := Write(track, FormatVTT)
	if err != nil {
		t.Fatal(err)
	}
	roundtrip, err := ParseVTT(data)
	if err != nil {
		t.Fatal(err)
	}
	if roundtrip.Cues[0].StartMS != 1250 || roundtrip.Cues[0].EndMS != 3750 || roundtrip.Cues[0].Text != "hello" {
		t.Fatalf("VTT drift: %#v", roundtrip.Cues[0])
	}
}

func TestParserErrorsIncludeCodeAndParsedCount(t *testing.T) {
	tests := []struct {
		name   string
		parse  func([]byte) (*Track, error)
		data   string
		code   string
		parsed int
	}{
		{"truncated_srt", ParseSRT, "1\n00:00:00,000 --> 00:00:01,000\nok\n\n2\n00:00:02,000 -->", "file_truncated", 1},
		{"missing_events", ParseASS, "[Script Info]\n", "missing_events_section", 0},
		{"short_dialogue", ParseASS, "[Events]\nFormat: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text\nDialogue: 0,0:00:01.00,0:00:02.00\n", "dialogue_fields_short", 0},
		{"truncated_ass", ParseASS, "[Events]\nFormat: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text\nDialogue: 0,0:00:01.00,0:00:02.00", "file_truncated", 0},
		{"overflow", ParseVTT, "WEBVTT\n\n1\n100:00:00.000 --> 100:00:01.000\nx\n", "timeline_overflow", 0},
		{"overflow_srt", ParseSRT, "1\n100:00:00,000 --> 100:00:01,000\nx\n", "timeline_overflow", 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := test.parse([]byte(test.data))
			parseErr, ok := err.(*ParseError)
			if !ok {
				t.Fatalf("error type=%T value=%v", err, err)
			}
			if parseErr.Code != test.code || parseErr.ParsedCues != test.parsed {
				t.Fatalf("got %s/%d want %s/%d", parseErr.Code, parseErr.ParsedCues, test.code, test.parsed)
			}
		})
	}
}

func TestPrepareForFormatDropsDrawingExplicitly(t *testing.T) {
	track := &Track{Format: FormatASS, Cues: []Cue{
		{Index: 1, StartMS: 0, EndMS: 1000, Text: `{\p1}m 0 0 l 10 10`},
		{Index: 2, StartMS: 2000, EndMS: 3000, Text: "dialogue"},
	}}
	Normalize(track)
	PrepareForFormat(track, FormatSRT)
	if track.Cues[0].Reason != "unsupported_ass_drawing" || track.Cues[1].Reason != "" {
		t.Fatalf("unexpected reasons: %#v", track.Cues)
	}
	data, err := Write(track, FormatSRT)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), `{\p`) || !strings.Contains(string(data), "dialogue") {
		t.Fatalf("drawing was not filtered: %s", data)
	}
}

func TestNormalizeIsReentrant(t *testing.T) {
	track := &Track{Cues: []Cue{
		{Index: 1, StartMS: 1000, EndMS: 3000},
		{Index: 2, StartMS: 2000, EndMS: 4000},
	}}
	Normalize(track)
	if track.Cues[0].OverlapGroup != 1 {
		t.Fatal("expected overlap group")
	}
	track.Cues[1].StartMS = 5000
	track.Cues[1].EndMS = 6000
	Normalize(track)
	if track.Cues[0].OverlapGroup != 0 || track.Cues[1].OverlapGroup != 0 {
		t.Fatalf("stale overlap group: %#v", track.Cues)
	}
}

func TestNormalizeStableForEqualStarts(t *testing.T) {
	track := &Track{Cues: []Cue{
		{Index: 3, StartMS: 1000, EndMS: 2000, Text: "third"},
		{Index: 1, StartMS: 1000, EndMS: 2000, Text: "first"},
		{Index: 2, StartMS: 1000, EndMS: 2000, Text: "second"},
	}}
	Normalize(track)
	if track.Cues[0].Index != 3 || track.Cues[1].Index != 1 || track.Cues[2].Index != 2 {
		t.Fatalf("sort was not stable: %#v", track.Cues)
	}
}

func TestASSTimeRolloverStaysCentisecondPrecise(t *testing.T) {
	for _, ms := range []int64{0, 995, 9995, 163995, 3599995} {
		formatted := FormatASSTime(ms)
		parsed, err := ParseASSTime(formatted)
		if err != nil {
			t.Fatalf("ms=%d formatted=%q: %v", ms, formatted, err)
		}
		drift := parsed - ms
		if drift < 0 {
			drift = -drift
		}
		if drift > 5 {
			t.Fatalf("ms=%d formatted=%q parsed=%d drift=%d", ms, formatted, parsed, drift)
		}
		if strings.Count(formatted, ".") != 1 || len(strings.SplitN(formatted, ".", 2)[1]) != 2 {
			t.Fatalf("ASS timestamp is not centisecond precise: %q", formatted)
		}
	}
}
