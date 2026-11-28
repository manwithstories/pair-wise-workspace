package subtitle

import (
	"fmt"
	"strings"

	"presspack/internal/core"
)

// RenderVTT serializes cues as a WebVTT document (segment-local times).
func RenderVTT(cues []Cue) []byte {
	var b strings.Builder
	b.WriteString("WEBVTT\n")
	for _, c := range cues {
		b.WriteString("\n")
		b.WriteString(formatVTTTime(c.Start))
		b.WriteString(" --> ")
		b.WriteString(formatVTTTime(c.End))
		b.WriteString("\n")
		b.WriteString(c.Text)
		b.WriteString("\n")
	}
	return []byte(b.String())
}

func formatVTTTime(t core.Time) string {
	ms := t.Millis()
	if ms < 0 {
		ms = 0
	}
	return fmt.Sprintf("%02d:%02d:%02d.%03d",
		ms/3600000, ms/60000%60, ms/1000%60, ms%1000)
}
