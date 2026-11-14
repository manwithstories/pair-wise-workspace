package subtitle

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var assDrawingPattern = regexp.MustCompile(`\{[^}]*\\[pP][1-9][0-9]*`)

func PrepareForFormat(t *Track, target Format) {
	if t == nil || target == FormatASS {
		return
	}
	for i := range t.Cues {
		cue := &t.Cues[i]
		if cue.Invalid || cue.Reason != "" {
			continue
		}
		if assDrawingPattern.MatchString(cue.Text) {
			cue.Reason = "unsupported_ass_drawing"
		}
	}
}

func Eligible(cues []Cue) []Cue {
	out := make([]Cue, 0, len(cues))
	for _, cue := range cues {
		if !cue.Invalid && cue.Reason == "" {
			out = append(out, cue)
		}
	}
	return out
}

func Write(t *Track, target Format) ([]byte, error) {
	if t == nil {
		return nil, fmt.Errorf("nil track")
	}
	cues := Eligible(t.Cues)
	switch target {
	case FormatSRT:
		return writeSRT(cues), nil
	case FormatASS:
		return writeASS(cues), nil
	case FormatVTT:
		return writeVTT(cues), nil
	default:
		return nil, fmt.Errorf("unsupported output format %q", target)
	}
}

func writeSRT(cues []Cue) []byte {
	var b strings.Builder
	b.Grow(len(cues) * 96)
	for i, cue := range cues {
		fmt.Fprintf(&b, "%d\n%s --> %s\n%s\n\n", i+1,
			FormatSRTTime(cue.StartMS), FormatSRTTime(cue.EndMS), normalizeText(cue.Text))
	}
	return []byte(b.String())
}

func writeASS(cues []Cue) []byte {
	var b strings.Builder
	b.Grow(len(cues)*128 + 256)
	b.WriteString("[Script Info]\nScriptType: v4.00+\nWrapStyle: 0\n\n")
	b.WriteString("[Events]\nFormat: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text\n")
	for _, cue := range cues {
		layer := cue.Layer
		if layer == "" || layer == "Marked=0" {
			layer = "0"
		}
		style := cue.Style
		if style == "" {
			style = "Default"
		}
		fmt.Fprintf(&b, "Dialogue: %s,%s,%s,%s,%s,%s,%s,%s,%s,%s\n",
			layer, FormatASSTime(cue.StartMS), FormatASSTime(cue.EndMS), style, cue.Name,
			margin(cue.MarginL), margin(cue.MarginR), margin(cue.MarginV), cue.Effect,
			escapeASSText(normalizeText(cue.Text)))
	}
	return []byte(b.String())
}

func writeVTT(cues []Cue) []byte {
	var b strings.Builder
	b.Grow(len(cues)*104 + 32)
	b.WriteString("WEBVTT\n\n")
	for i, cue := range cues {
		if cue.ID != "" {
			b.WriteString(cue.ID)
		} else {
			b.WriteString(strconv.Itoa(i + 1))
		}
		fmt.Fprintf(&b, "\n%s --> %s\n%s\n\n",
			FormatVTTTime(cue.StartMS), FormatVTTTime(cue.EndMS), normalizeText(cue.Text))
	}
	return []byte(b.String())
}

func normalizeText(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	return strings.ReplaceAll(text, "\r", "\n")
}

func escapeASSText(text string) string {
	return strings.ReplaceAll(text, "\n", `\N`)
}

func margin(value string) string {
	if value == "" {
		return "0"
	}
	return value
}
