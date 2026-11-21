// Package mp4 parses ISO base media files into a validated box tree,
// re-serializes the tree, and compares the result with the input.
//
// The walker in tree.go validates every declared box length against its
// parent before handing a payload to the typed unmarshaller, so truncated
// boxes, illegal lengths and unknown box types are all reported instead of
// being silently skipped.  summary.go extracts the fields that the round-trip
// comparison and the keyframe index consume.
package mp4

import mp4 "github.com/abema/go-mp4"

// IsOpaque reports whether a box keeps its payload as raw bytes rather than
// decoding it.  It exists so callers can explain why an mdat box carries no
// typed payload without duplicating the parser's policy table.
func IsOpaque(typ mp4.BoxType) bool {
	return opaque[typ]
}
