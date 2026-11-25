package pipeline

import (
	"encoding/hex"
	"fmt"
	"strings"

	"archivecheck/internal/demux"
)

// canonicalMapping renders a deterministic description of the surviving tracks
// and their sample boundaries. It feeds the cache key, so identical recovery
// decisions always produce an identical mapping string.
func canonicalMapping(tracks []demux.TrackSamples) string {
	var b strings.Builder
	for _, tr := range tracks {
		first, last := -1, 0
		if len(tr.Samples) > 0 {
			first = tr.Samples[0].Index
			last = tr.Samples[len(tr.Samples)-1].Index + 1
		}
		fmt.Fprintf(&b, "track=%d,codec=%s,range=%d:%d,tb=%d/%d;",
			tr.Meta.Track, tr.Meta.Codec, first, last,
			tr.Meta.TimeBase.Num, tr.Meta.TimeBase.Den)
	}
	return b.String()
}

func hashToHex(d [32]byte) string {
	return hex.EncodeToString(d[:])
}
