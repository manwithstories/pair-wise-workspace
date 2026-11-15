// Package serialize writes unified cues back to SRT, WebVTT, ASS and
// TTML. Writers are the exact inverse of the parse layer so round-trip
// verification can re-parse output with the same code.
package serialize

import (
	"fmt"
	"math/big"
	"strconv"
	"strings"

	"subtool/internal/model"
)

// Format names shared by CLI flags.
const (
	SRT  = "srt"
	VTT  = "vtt"
	ASS  = "ass"
	TTML = "ttml"
)

// Formats lists every supported format in stable order.
var Formats = []string{SRT, VTT, ASS, TTML}

// Write renders cues in the requested format. Cues are emitted in
// (start, id) order with sequential numbering where the format needs it.
func Write(cues []model.Cue, format string) (string, error) {
	sorted := make([]model.Cue, len(cues))
	copy(sorted, cues)
	model.SortByStart(sorted)
	switch format {
	case SRT:
		return writeSRT(sorted), nil
	case VTT:
		return writeVTT(sorted), nil
	case ASS:
		return writeASS(sorted), nil
	case TTML:
		return writeTTML(sorted), nil
	}
	return "", fmt.Errorf("unknown format %q", format)
}

// formatClock renders rational ms as HH:MM:SS plus a fraction with the
// given separator and precision, rounding half-up at the last digit.
func formatClock(ms *big.Rat, sep string, digits int) string {
	scale := int64(1)
	for i := 0; i < digits; i++ {
		scale *= 10
	}
	// total units of 10^-digits seconds, rounded half up
	scaled := new(big.Rat).Mul(ms, big.NewRat(scale, 1000))
	num, den := scaled.Num(), scaled.Denom()
	q := new(big.Int).Quo(num, den)
	rem := new(big.Int).Rem(num, den)
	if new(big.Int).Mul(rem, big.NewInt(2)).Cmp(den) >= 0 {
		q.Add(q, big.NewInt(1))
	}
	units := q.Int64()
	frac := units % scale
	total := units / scale
	ss := total % 60
	mm := (total / 60) % 60
	hh := total / 3600
	return fmt.Sprintf("%02d:%02d:%02d%s%0*d", hh, mm, ss, sep, digits, frac)
}

func writeSRT(cues []model.Cue) string {
	var b strings.Builder
	for i, c := range cues {
		index := i + 1
		if n, err := strconv.Atoi(c.ID); err == nil && n > 0 {
			index = n // preserve numeric IDs so SRT round-trips losslessly
		}
		fmt.Fprintf(&b, "%d\n%s --> %s\n%s\n\n",
			index,
			formatClock(c.Start, ",", 3),
			formatClock(c.End, ",", 3),
			c.Text)
	}
	return b.String()
}

func writeVTT(cues []model.Cue) string {
	var b strings.Builder
	b.WriteString("WEBVTT\n\n")
	for _, c := range cues {
		fmt.Fprintf(&b, "%s\n%s --> %s\n%s\n\n",
			c.ID,
			formatClock(c.Start, ".", 3),
			formatClock(c.End, ".", 3),
			c.Text)
	}
	return b.String()
}

const assHeader = `[Script Info]
ScriptType: v4.00+
PlayResX: 1920
PlayResY: 1080

[V4+ Styles]
Format: Name, Fontname, Fontsize, PrimaryColour, BackColour, Bold, Italic, BorderStyle, Outline, Shadow, Alignment, MarginL, MarginR, MarginV, Encoding
Style: Default,Arial,48,&H00FFFFFF,&H00000000,0,0,1,2,0,2,30,30,30,1

[Events]
Format: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text
`

func writeASS(cues []model.Cue) string {
	var b strings.Builder
	b.WriteString(assHeader)
	for _, c := range cues {
		// ASS classically uses centiseconds; we emit millisecond
		// precision (accepted by the parser) to keep round-trip
		// drift under 1ms on the rational timeline.
		start := formatClock(c.Start, ".", 3)
		end := formatClock(c.End, ".", 3)
		fmt.Fprintf(&b, "Dialogue: %d,%s,%s,Default,,0,0,0,,%s\n",
			c.Layer, start, end,
			strings.ReplaceAll(c.Text, "\n", `\N`))
	}
	return b.String()
}

func writeTTML(cues []model.Cue) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<tt xmlns="http://www.w3.org/ns/ttml" xmlns:ttp="http://www.w3.org/ns/ttml#parameter" ttp:tickRate="1000">` + "\n")
	b.WriteString("<body><div>\n")
	for _, c := range cues {
		fmt.Fprintf(&b, `<p xml:id="%s" begin="%s" end="%s">%s</p>`+"\n",
			c.ID,
			formatClock(c.Start, ".", 3),
			formatClock(c.End, ".", 3),
			escapeXML(c.Text))
	}
	b.WriteString("</div></body>\n</tt>\n")
	return b.String()
}

var xmlEscaper = strings.NewReplacer(
	"&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;",
)

func escapeXML(s string) string { return xmlEscaper.Replace(s) }
