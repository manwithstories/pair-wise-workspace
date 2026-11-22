package fixture

import (
	"fmt"
	"strings"
)

// SRT generates a SubRip document with cueCount cues at the given frame rate.
// Times are derived from exact nanosecond values so the same seed reproduces
// byte-identical output.
func SRT(cueCount int, startNs int64, stepNs int64, durNs int64) string {
	var b strings.Builder
	for i := 0; i < cueCount; i++ {
		start := startNs + int64(i)*stepNs
		end := start + durNs
		fmt.Fprintf(&b, "%d\n", i+1)
		fmt.Fprintf(&b, "%s --> %s\n", srtTime(start), srtTime(end))
		fmt.Fprintf(&b, "cue %d commentary before the cut\n", i+1)
		b.WriteString("\n")
	}
	return b.String()
}

// VTT generates a WebVTT document with identifiers and cue settings.
func VTT(cueCount int, startNs, stepNs, durNs int64) string {
	var b strings.Builder
	b.WriteString("WEBVTT\nKind: captions\n\n")
	for i := 0; i < cueCount; i++ {
		start := startNs + int64(i)*stepNs
		end := start + durNs
		fmt.Fprintf(&b, "cue-%d\n", i+1)
		fmt.Fprintf(&b, "%s --> %s align:start\n", vttTime(start), vttTime(end))
		fmt.Fprintf(&b, "remote narrator line %d\n\n", i+1)
	}
	return b.String()
}

func srtTime(ns int64) string {
	ms := ns / 1_000_000
	h := ms / 3_600_000
	ms -= h * 3_600_000
	m := ms / 60_000
	ms -= m * 60_000
	s := ms / 1000
	ms -= s * 1000
	return fmt.Sprintf("%02d:%02d:%02d,%03d", h, m, s, ms)
}

func vttTime(ns int64) string {
	ms := ns / 1_000_000
	h := ms / 3_600_000
	ms -= h * 3_600_000
	m := ms / 60_000
	ms -= m * 60_000
	s := ms / 1000
	ms -= s * 1000
	return fmt.Sprintf("%02d:%02d:%02d.%03d", h, m, s, ms)
}
