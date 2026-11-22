package mp4clip

import "github.com/abema/go-mp4"

// sttsEntries expands clip samples into lossless time-to-sample entries: one
// run per sample so varying deltas are preserved exactly.
func sttsEntries(samples []Sample) []mp4.SttsEntry {
	entries := make([]mp4.SttsEntry, len(samples))
	for i, s := range samples {
		entries[i] = mp4.SttsEntry{SampleCount: 1, SampleDelta: s.Delta}
	}
	return entries
}
