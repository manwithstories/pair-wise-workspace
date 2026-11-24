package pipeline

import (
	"fmt"

	"mediarun/internal/index"
	"mediarun/internal/model"
	"mediarun/internal/parse"
	"mediarun/internal/testkit"
)

type indexResult struct {
	rate      float64
	keyframes int
}

func runIndex(media *testkit.Media) (*indexResult, error) {
	movie, err := parse.ParseMovie(media.Bytes)
	if err != nil {
		return nil, err
	}
	queries := 0
	hits := 0
	for _, track := range movie.Tracks {
		if track.Handler != "vide" {
			continue
		}
		idx, err := index.Build(track)
		if err != nil {
			return nil, err
		}
		lookup := index.LookupFromTrack(track)
		set := index.GenerateQueries(idx)
		if len(set) == 0 {
			continue
		}
		// Deliberately corrupt the in-memory copy for every second track
		// so the rebuild path runs and is measured.
		if track.ID%2 == 0 && len(idx.Entries) > 0 {
			idx.Entries[0].ByteLen++
		}
		fresh, cov, err := index.Batch(idx, set, lookup, func() (*index.Index, error) {
			return index.Build(track)
		})
		if err != nil {
			return nil, err
		}
		_ = fresh
		queries += cov.Queries
		hits += cov.Hits
	}
	if queries == 0 {
		return nil, fmt.Errorf("%w: no keyframe queries", model.ErrInvalid)
	}
	return &indexResult{rate: float64(hits) / float64(queries)}, nil
}
