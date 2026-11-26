package subtitle

import (
	"bytes"
	"io"
	"strings"
	"time"

	"github.com/asticode/go-astisub"
)

// ToAstisub converts cues to the third-party subtitle model.
func ToAstisub(cues []Cue) *astisub.Subtitles {
	out := astisub.NewSubtitles()
	for _, cue := range cues {
		out.Items = append(out.Items, &astisub.Item{
			StartAt: time.Duration(cue.StartMS) * time.Millisecond,
			EndAt:   time.Duration(cue.EndMS) * time.Millisecond,
			Lines:   []astisub.Line{{Items: []astisub.LineItem{{Text: cue.Text}}}},
		})
	}
	return out
}

// CuesFromAstisub converts astisub items back to millisecond cues.
func CuesFromAstisub(in *astisub.Subtitles) []Cue {
	var cues []Cue
	for _, item := range in.Items {
		var lines []string
		for _, line := range item.Lines {
			var text strings.Builder
			for _, li := range line.Items {
				text.WriteString(li.Text)
			}
			lines = append(lines, text.String())
		}
		cues = append(cues, Cue{
			StartMS: int64(item.StartAt / time.Millisecond),
			EndMS:   int64(item.EndAt / time.Millisecond),
			Text:    strings.Join(lines, "\n"),
		})
	}
	return cues
}

// Encode writes valid cues in SRT or WebVTT through astisub.
func Encode(cues []Cue, format Format) ([]byte, error) {
	s := ToAstisub(cues)
	var buf bytes.Buffer
	var err error
	if format == FormatVTT {
		err = s.WriteToWebVTT(&buf)
	} else {
		err = s.WriteToSRT(&buf)
	}
	return buf.Bytes(), err
}

// ReadAstisub uses the dependency's parser for round-trip verification.
func ReadAstisub(r io.Reader, format Format) ([]Cue, error) {
	var (
		subs *astisub.Subtitles
		err  error
	)
	if format == FormatVTT {
		subs, err = astisub.ReadFromWebVTT(r)
	} else {
		subs, err = astisub.ReadFromSRT(r)
	}
	if err != nil {
		return nil, err
	}
	return CuesFromAstisub(subs), nil
}

// RoundTrip encodes and decodes cues and reports exact text/boundary equality.
func RoundTrip(cues []Cue, format Format) ([]Cue, []byte, error) {
	encoded, err := Encode(cues, format)
	if err != nil {
		return nil, nil, err
	}
	got, err := ReadAstisub(bytes.NewReader(encoded), format)
	if err != nil {
		return nil, encoded, err
	}
	return got, encoded, nil
}

// Equal reports whether two round-tripped cue sets preserve milliseconds/text.
func Equal(a, b []Cue) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].StartMS != b[i].StartMS || a[i].EndMS != b[i].EndMS || a[i].Text != b[i].Text {
			return false
		}
	}
	return true
}
