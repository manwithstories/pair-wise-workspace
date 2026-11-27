// Package subtitle parses SRT/WebVTT cues into the unified nanosecond time
// base and reconciles overlapping or out-of-order cues against the clock
// repair rules.
package subtitle

import (
	"fmt"
	"io"
	"strings"

	"github.com/asticode/go-astisub"

	"avrepair/internal/clock"
)

// Format identifies a subtitle container.
type Format string

const (
	FormatSRT    Format = "srt"
	FormatWebVTT Format = "vtt"
)

// Cue is one subtitle interval in the unified nanosecond time base.
type Cue struct {
	Index   int    `json:"index"`
	StartNS int64  `json:"start_ns"`
	EndNS   int64  `json:"end_ns"`
	Text    string `json:"text"`
}

// Track is a named subtitle stream plus the structured defects found in it.
type Track struct {
	Name   string          `json:"name"`
	Format Format          `json:"format"`
	Cues   []Cue           `json:"cues"`
	Errors []*clock.TrackError `json:"errors,omitempty"`
}

// Parse decodes a subtitle stream. format selects the SRT or WebVTT reader;
// parser failures are mapped to the shared CodeSubtitleParse error model.
func Parse(name string, format Format, r io.Reader) (*Track, error) {
	var (
		sub *astisub.Subtitles
		err error
	)
	switch format {
	case FormatSRT:
		sub, err = astisub.ReadFromSRT(r)
	case FormatWebVTT:
		sub, err = astisub.ReadFromWebVTT(r)
	default:
		return nil, clock.NewError(clock.CodeSubtitleParse, name, clock.Interval{},
			fmt.Sprintf("unsupported subtitle format %q", format))
	}
	if err != nil {
		return nil, clock.NewError(clock.CodeSubtitleParse, name, clock.Interval{},
			fmt.Sprintf("%s parse failure: %v", format, err))
	}
	tr := &Track{Name: name, Format: format}
	for i, item := range sub.Items {
		text := joinLines(item.Lines)
		tr.Cues = append(tr.Cues, Cue{
			Index:   i,
			StartNS: int64(item.StartAt),
			EndNS:   int64(item.EndAt),
			Text:    text,
		})
	}
	return tr, nil
}

// joinLines flattens astisub lines into plain text, one physical line each.
func joinLines(lines []astisub.Line) string {
	parts := make([]string, 0, len(lines))
	for _, ln := range lines {
		var b strings.Builder
		for _, li := range ln.Items {
			b.WriteString(li.Text)
		}
		parts = append(parts, b.String())
	}
	return strings.Join(parts, "\n")
}
