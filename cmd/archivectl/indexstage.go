package main

import (
	"path/filepath"

	"archive/internal/index"
	"archive/internal/media"
	"archive/internal/timeline"
)

// timelineOut is the repaired-track bundle handed to the index stage.
type timelineOut struct {
	tracks []*timeline.RepairedTrack
	syncUs int64
}

// mediaOut locates the generated MP4 and its real size.
type mediaOut struct {
	path      string
	availByte int64
}

// indexOut is the committed index summary.
type indexOut struct {
	entries int
	sha256  string
}

// buildIndex parses the MP4 sample layout, streams repaired samples
// through the index builder and commits one segment per track. Truncated
// media fails the node with a locatable corrupt error before any commit.
func buildIndex(t *timelineOut, mo *mediaOut, work string) (*indexOut, error) {
	infos, err := readMP4(mo.path)
	if err != nil {
		return nil, err
	}
	if len(infos) != len(t.tracks) {
		return nil, media.Corruptf("demo.index", "", -1, "trak count mismatch")
	}
	store, err := index.Open(filepath.Join(work, "index"))
	if err != nil {
		return nil, err
	}
	out := &indexOut{}
	for i, rt := range t.tracks {
		if infos[i].timescale != rt.Timescale {
			return nil, media.Corruptf("demo.index", rt.TrackID, -1,
				"timescale mismatch between manifest and mdhd")
		}
		b := index.NewBuilder(rt, uint64(mo.availByte))
		for _, s := range rt.Samples {
			if s.Index >= len(infos[i].offsets) {
				return nil, media.Corruptf("demo.index", rt.TrackID, int64(s.Index),
					"sample missing from container")
			}
			off := infos[i].offsets[s.Index]
			if err := b.Add(s, off); err != nil {
				return nil, err // truncation: no out-of-range index committed
			}
		}
		data := index.EncodeEntries(b.Entries())
		if _, err := store.CommitSegment(rt.TrackID, data); err != nil {
			return nil, err
		}
		out.entries += len(b.Entries())
	}
	sha, err := store.Finalize()
	if err != nil {
		return nil, err
	}
	out.sha256 = sha
	return out, nil
}
