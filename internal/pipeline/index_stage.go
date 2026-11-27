package pipeline

import (
	"fmt"
	"path/filepath"

	"avrepair/internal/index"
	"avrepair/internal/journal"
	"avrepair/internal/media"
)

func (c *runContext) buildIndex() (*index.Index, error) {
	idx, err := index.Build(c.timeline, index.BuildOptions{
		InputHash: c.digest,
		OutputOf: func(seg *media.Segment) (string, bool) {
			if seg.Output == "" {
				return "", false
			}
			return seg.Output, true
		},
		SourceHashOf: func(seg *media.Segment) string { return seg.SourceSHA },
	})
	if err != nil {
		return nil, err
	}
	// The index carries the run's result hash so a reader can tie the
	// committed map back to the repaired timeline.
	idx.ResultHash = c.hash
	path := filepath.Join(c.opt.OutputDir, "index.json")
	sum, err := index.WriteAtomic(path, idx, c.opt.OutputDir)
	if err != nil {
		return nil, err
	}
	c.report.IndexEntries = len(idx.Entries)
	c.report.IndexPath = path
	c.recordSeq++
	if err := c.store.AppendRecord(journal.Record{
		Seq: c.recordSeq, Stage: StageIndex, InputDigest: c.digest, OutputSHA256: sum,
		Detail: fmt.Sprintf("%d entries", len(idx.Entries)),
	}); err != nil {
		return nil, err
	}
	return idx, nil
}
