package subtitle

import (
	"testing"

	"mediad/diag"
)

const srtSample = "1\r\n" +
	"00:00:01,000 --> 00:00:04,000\r\n" +
	"Hello\r\n\r\n" +
	"2\r\n" +
	"00:00:05,500 --> 00:00:06,250\r\n" +
	"World\r\n" +
	"second line\r\n\r\n"

func feedChunks(t *testing.T, p interface {
	Write([]byte) error
}, data []byte, sizes []int) {
	t.Helper()
	pos := 0
	for pos < len(data) {
		n := sizes[0]
		if len(sizes) > 1 {
			n = sizes[pos%len(sizes)]
		}
		if pos+n > len(data) {
			n = len(data) - pos
		}
		if err := p.Write(data[pos : pos+n]); err != nil {
			t.Fatalf("write at %d: %v", pos, err)
		}
		pos += n
	}
}

func TestSRTChunkBoundariesIdentical(t *testing.T) {
	patterns := [][]int{
		{len(srtSample)}, // whole buffer
		{1},              // one byte at a time
		{3},
		{7, 1, 13},
		{64, 2},
	}
	var reference []Cue
	for i, sizes := range patterns {
		p := NewSRT("sub-en", diag.DefaultLimits())
		feedChunks(t, p, []byte(srtSample), sizes)
		cues, err := p.Close()
		if err != nil {
			t.Fatalf("pattern %d: %v", i, err)
		}
		if i == 0 {
			reference = cues
			continue
		}
		if len(cues) != len(reference) {
			t.Fatalf("pattern %d: got %d cues, want %d", i, len(cues), len(reference))
		}
		for j := range reference {
			if cues[j] != reference[j] {
				t.Fatalf("pattern %d cue %d mismatch:\n got %+v\nwant %+v", i, j, cues[j], reference[j])
			}
		}
	}
	if len(reference) != 2 || reference[0].Start != 1000 || reference[0].End != 4000 ||
		reference[1].Start != 5500 || reference[1].End != 6250 ||
		reference[1].Text != "World\nsecond line" {
		t.Fatalf("unexpected cues: %+v", reference)
	}
}

func TestSRTTruncationReportsOffset(t *testing.T) {
	// Full second cue header present but EOF inside the payload and no newline.
	bad := "1\n00:00:01,000 --> 00:00:02,000\nok\n\n2\n00:00:03,000 --> 00:00:04,000\ncut"
	p := NewSRT("sub-de", diag.DefaultLimits())
	if err := p.Write([]byte(bad)); err != nil {
		t.Fatal(err)
	}
	// Trailing unterminated line: truncation at the payload line's first byte.
	_, err := p.Close()
	de, ok := err.(*diag.Error)
	if !ok || de.Code != diag.ESubtitleTruncated || de.Track != "sub-de" {
		t.Fatalf("want located truncation, got %v", err)
	}
	wantOff := int64(len("1\n00:00:01,000 --> 00:00:02,000\nok\n\n2\n00:00:03,000 --> 00:00:04,000\n"))
	if de.Offset != wantOff {
		t.Fatalf("offset = %d, want %d", de.Offset, wantOff)
	}
}

func TestSRTIndexWithoutHeader(t *testing.T) {
	p := NewSRT("t", diag.DefaultLimits())
	_ = p.Write([]byte("7\n\n"))
	_, err := p.Close()
	if diag.CodeOf(err) != diag.ESubtitleTruncated {
		t.Fatalf("want E_SUBTITLE_TRUNCATED, got %v", err)
	}
}

func TestSRTSyntaxError(t *testing.T) {
	p := NewSRT("t", diag.DefaultLimits())
	err := p.Write([]byte("oops\n"))
	if diag.CodeOf(err) != diag.ESubtitleSyntax || diag.OffsetOf(err) != 0 {
		t.Fatalf("want syntax error at offset 0, got %v", err)
	}
}

func TestTracksIndependentAfterFailure(t *testing.T) {
	bad := NewSRT("broken", diag.DefaultLimits())
	good := NewSRT("fine", diag.DefaultLimits())
	if err := bad.Write([]byte("nope\n")); diag.CodeOf(err) != diag.ESubtitleSyntax {
		t.Fatalf("expected broken track error, got %v", err)
	}
	if err := good.Write([]byte(srtSample)); err != nil {
		t.Fatalf("good track must stay independent: %v", err)
	}
	cues, err := good.Close()
	if err != nil || len(cues) != 2 {
		t.Fatalf("good track result: %d cues, err=%v", len(cues), err)
	}
}

const vttSample = "WEBVTT - demo\r\n\r\n" +
	"NOTE a comment block\r\nspanning lines\r\n\r\n" +
	"cue-1\r\n" +
	"00:00:01.000 --> 00:00:04.000 line:90%\r\n" +
	"Hello VTT\r\n\r\n" +
	"00:05.500 --> 00:06.250\r\n" +
	"no id here\r\n"

func TestVTTParsing(t *testing.T) {
	p := NewVTT("vtt-en", diag.DefaultLimits())
	feedChunks(t, p, []byte(vttSample), []int{5})
	cues, err := p.Close()
	if err != nil {
		t.Fatal(err)
	}
	if len(cues) != 2 {
		t.Fatalf("got %d cues: %+v", len(cues), cues)
	}
	if cues[0].Start != 1000 || cues[0].End != 4000 || cues[0].Text != "Hello VTT" {
		t.Fatalf("cue1 = %+v", cues[0])
	}
	// MM:SS.mmm normalized to HH:MM:SS.mmm.
	if cues[1].Start != 5500 || cues[1].End != 6250 || cues[1].Text != "no id here" {
		t.Fatalf("cue2 = %+v", cues[1])
	}
}

func TestVTTMissingHeader(t *testing.T) {
	p := NewVTT("v", diag.DefaultLimits())
	err := p.Write([]byte("00:00:01.000 --> 00:00:02.000\nx\n"))
	if diag.CodeOf(err) != diag.ESubtitleSyntax {
		t.Fatalf("missing header must fail, got %v", err)
	}
}

func TestVTTTruncatedCue(t *testing.T) {
	p := NewVTT("v", diag.DefaultLimits())
	_ = p.Write([]byte("WEBVTT\n\nid-9\n00:00:01.000 --> 00:00:02.000\ntext"))
	_, err := p.Close()
	if diag.CodeOf(err) != diag.ESubtitleTruncated {
		t.Fatalf("want truncation, got %v", err)
	}
}

func TestLineLimitEnforced(t *testing.T) {
	lim := diag.DefaultLimits()
	lim.MaxCueLines = 2
	p := NewSRT("capped", lim)
	// Two blank lines stay under budget; the third line trips the hard cap
	// before any format parsing happens.
	err := p.Write([]byte("\n\n\n"))
	if diag.CodeOf(err) != diag.EParam {
		t.Fatalf("line budget must produce E_PARAM, got %v", err)
	}
}

func TestEndBeforeStartRejected(t *testing.T) {
	p := NewSRT("t", diag.DefaultLimits())
	err := p.Write([]byte("1\n00:00:05,000 --> 00:00:01,000\nx\n\n"))
	if diag.CodeOf(err) != diag.ETimecode {
		t.Fatalf("inverted range must be E_TIMECODE, got %v", err)
	}
}
