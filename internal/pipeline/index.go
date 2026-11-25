package pipeline

import (
	"fmt"

	"mediacheck/internal/ingest"
	"mediacheck/internal/media"
)

// crossCheckKeyIndex compares parsed keyframe units with JSON index entries and
// returns the index-declared total duration on the 90 kHz base.
func crossCheckKeyIndex(vcl []ingest.Unit, idx *ingest.KeyframeIndex) (int64, error) {
	if idx == nil {
		return 0, nil
	}
	if err := idx.ValidateTimeline(); err != nil {
		return 0, err
	}
	var keys []ingest.Unit
	for _, u := range vcl {
		if u.Keyframe {
			keys = append(keys, u)
		}
	}
	if len(keys) != len(idx.Entries) {
		return 0, media.E(media.StageManifest, media.KindValidation, -1,
			fmt.Errorf("parsed keyframes=%d index entries=%d", len(keys), len(idx.Entries)))
	}
	for i, e := range idx.Entries {
		if keys[i].PTS != e.PTS {
			return 0, media.E(media.StageManifest, media.KindValidation, e.Offset,
				fmt.Errorf("key %d PTS parsed=%d index=%d", i, keys[i].PTS, e.PTS)).WithPTS(e.PTS)
		}
		if keys[i].Offset != e.Offset {
			return 0, media.E(media.StageManifest, media.KindValidation, e.Offset,
				fmt.Errorf("key %d offset parsed=%d index=%d", i, keys[i].Offset, e.Offset))
		}
	}
	if len(idx.Entries) == 0 {
		return 0, media.E(media.StageIndex, media.KindValidation, -1, fmt.Errorf("empty index"))
	}
	last := idx.Entries[len(idx.Entries)-1]
	if idx.KeyframeIntervalTicks > 0 {
		return last.PTS + idx.KeyframeIntervalTicks, nil
	}
	return last.PTS, nil
}
