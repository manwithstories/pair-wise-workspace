// Package mp4 parses ISO base media files into a validated box tree,
// re-serializes the tree, and compares the result with the input.
//
// The walker in tree.go validates every declared box length against its
// parent before handing a payload to the typed unmarshaller, so truncated
// boxes, illegal lengths and unknown box types are all reported instead of
// being silently skipped.  summary.go extracts the fields that the round-trip
// comparison and the keyframe index consume.
package mp4
