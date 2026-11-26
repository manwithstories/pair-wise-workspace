package recovery

import "fmt"

// Checkpointer commits fixed-size chunks of work to the index. A chunk
// that was never committed is always redone; a committed chunk is never
// committed twice.
type Checkpointer struct {
	idx *Index
	run int
}

// NewCheckpointer starts a new run over the index. The run number is one
// higher than any previously committed run.
func NewCheckpointer(idx *Index) *Checkpointer {
	run := 1
	for _, c := range idx.Chunks {
		if c.Run >= run {
			run = c.Run + 1
		}
	}
	return &Checkpointer{idx: idx, run: run}
}

// Run returns the current run number (1 for the first run).
func (c *Checkpointer) Run() int {
	return c.run
}

// Committed reports whether the chunk is already committed.
func (c *Checkpointer) Committed(chunk int) bool {
	_, ok := c.idx.Chunks[chunk]
	return ok
}

// Commit records a finished chunk. Re-committing identical results is a
// no-op; committing different results for the same chunk is an error.
func (c *Checkpointer) Commit(rec Record) error {
	rec.Type = "chunk"
	rec.Run = c.run
	if prev, ok := c.idx.Chunks[rec.Chunk]; ok {
		if prev.OutDigest == rec.OutDigest && prev.Segments == rec.Segments {
			return nil
		}
		return fmt.Errorf("chunk %d already committed with different results", rec.Chunk)
	}
	return c.idx.Append(&rec)
}

// Finish appends the terminal record carrying the overall results digest.
func (c *Checkpointer) Finish(resultsDigest string) error {
	if c.idx.Done != nil {
		if c.idx.Done.ResultsDigest == resultsDigest {
			return nil
		}
		return fmt.Errorf("run already finished with different results digest")
	}
	return c.idx.Append(&Record{Type: "done", ResultsDigest: resultsDigest})
}
