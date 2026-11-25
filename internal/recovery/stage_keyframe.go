package recovery

import (
	"encoding/json"

	"mediad/internal/codec"
)

func (e *exec) stageKeyframe() error {
	var actual []int
	globalOffset := 0
	for _, seg := range e.segs {
		for _, n := range seg.nals {
			if n.IDR {
				actual = append(actual, globalOffset+n.Index)
			}
		}
		globalOffset += len(seg.nals)
	}

	var expected []int
	for i := 0; i < globalOffset; i++ {
		if i%3 == 0 {
			expected = append(expected, i)
		}
	}
	hits, err := codec.ValidateKeyframeIndex(e.hls.Path, actual, expected)
	if err != nil {
		return err
	}
	matched, total, rate := codec.KeyframeScore(hits)
	e.sum.KeyframeMatched = matched
	e.sum.KeyframeTotal = total
	e.sum.KeyframeHit = rate

	data, err := json.MarshalIndent(hits, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFileAtomic(e.artifactPath("keyframes.json"), data); err != nil {
		return err
	}
	return e.finishStage("keyframe")
}
