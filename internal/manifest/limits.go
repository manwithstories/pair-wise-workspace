package manifest

import (
	"bytes"
	"strings"
)

// Bounds enforced while reading manifests and playlist references. They keep
// fixture-driven runs honest and prevent a hostile manifest from exhausting
// memory; the spec requires bounded file reads.
const (
	MaxManifestBytes = 4 << 20 // 4 MiB manifest cap
	MaxLineBytes     = 1 << 16 // 64 KiB per playlist line
	// DefaultSegmentCount is the synthesized window when a DASH template
	// carries no SegmentTimeline.
	DefaultSegmentCount = 24
)

// buildLineIndex maps representation/segment identifiers to the 1-based line
// where they are declared. DASH XML is attribute driven, so we scan textual
// lines for id="..." occurrences; HLS uses its own line-aware scanner.
func buildLineIndex(raw []byte) map[string]int {
	out := map[string]int{}
	lines := bytes.Split(raw, []byte("\n"))
	for i, ln := range lines {
		text := string(ln)
		if key := attrAfter(text, "id="); key != "" {
			if _, seen := out[key]; !seen {
				out[key] = i + 1
			}
		}
	}
	return out
}

// attrAfter extracts the first quoted attribute value following name.
func attrAfter(line, name string) string {
	idx := strings.Index(line, name)
	if idx < 0 {
		return ""
	}
	rest := line[idx+len(name):]
	q := strings.IndexAny(rest, "\"'")
	if q < 0 {
		return ""
	}
	quote := rest[q]
	tail := rest[q+1:]
	end := strings.IndexByte(tail, quote)
	if end < 0 {
		return ""
	}
	return tail[:end]
}
