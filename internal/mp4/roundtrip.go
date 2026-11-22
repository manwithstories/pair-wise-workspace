package mp4

import (
	"bytes"

	gomp4 "github.com/abema/go-mp4"
)

// RoundtripResult aggregates re-marshal verification over many fragments.
type RoundtripResult struct {
	Matched   int
	Total     int
	FirstDiff string // box type of the first mismatch, if any
}

// Percent returns the match ratio in percent.
func (r RoundtripResult) Percent() float64 {
	if r.Total == 0 {
		return 100
	}
	return 100 * float64(r.Matched) / float64(r.Total)
}

// RoundtripFragment re-marshals every parsed box of f with go-mp4 and
// compares the encoding against the original bytes.
func RoundtripFragment(f *Fragment, res *RoundtripResult) {
	for _, pb := range f.Parsed {
		res.Total++
		var buf bytes.Buffer
		if _, err := gomp4.Marshal(&buf, pb.Box, gomp4.Context{}); err != nil {
			if res.FirstDiff == "" {
				res.FirstDiff = pb.Type
			}
			continue
		}
		if bytes.Equal(buf.Bytes(), pb.Raw) {
			res.Matched++
		} else if res.FirstDiff == "" {
			res.FirstDiff = pb.Type
		}
	}
}
