package txn

import (
	"fmt"

	"mediarun/internal/model"
)

// CommitRun records a full begin/stage/commit cycle for a list of segments.
// It returns the new manifest; the caller decides whether to apply it.
type CommitRun struct {
	Dir      string
	Manifest *Manifest
	WAL      *WAL
}

// NewCommitRun opens the manifest and log for a run.
func NewCommitRun(dir string) (*CommitRun, error) {
	m, err := LoadManifest(dir)
	if err != nil {
		return nil, err
	}
	w, err := Open(dir)
	if err != nil {
		return nil, err
	}
	return &CommitRun{Dir: dir, Manifest: m, WAL: w}, nil
}

// Close closes the log.
func (c *CommitRun) Close() error { return c.WAL.Close() }

// Begin records the start of a stage.
func (c *CommitRun) Begin(stage string) error {
	return c.WAL.Append(Record{Op: OpBegin, Stage: stage})
}

// StageSegment writes a segment to disk and logs the intent. A staged
// segment is not committed until CommitSegment succeeds.
func (c *CommitRun) StageSegment(p *SegmentPayload) (SegmentCommit, error) {
	body, err := p.Encode()
	if err != nil {
		return SegmentCommit{}, err
	}
	hash, err := WriteSegment(c.Dir, p.Name, body)
	if err != nil {
		return SegmentCommit{}, err
	}
	if err := c.WAL.Append(Record{Op: OpStage, Segment: p.Name, Hash: hash, Stage: "compose"}); err != nil {
		return SegmentCommit{}, err
	}
	gaps := 0
	for _, g := range p.Gaps {
		gaps += g.Count
	}
	return SegmentCommit{
		Name:   p.Name,
		Start:  int64(p.Start),
		End:    int64(p.End),
		Events: len(p.Events),
		SHA256: hash,
		Gaps:   gaps,
	}, nil
}

// CommitSegment appends the commit record, then atomically replaces the
// manifest with one that includes the segment. The manifest write is the
// point of no return: before it the segment is recoverable garbage, after it
// the segment is part of the delivery and its hash must never change.
func (c *CommitRun) CommitSegment(seg SegmentCommit) error {
	if len(seg.SHA256) != 64 {
		return fmt.Errorf("%w: segment %q hash %q", model.ErrInvalid, seg.Name, seg.SHA256)
	}
	for _, existing := range c.Manifest.Segments {
		if existing.Name == seg.Name {
			if existing.SHA256 != seg.SHA256 {
				return fmt.Errorf("%w: committed segment %q hash would change", model.ErrCommit, seg.Name)
			}
			return nil // idempotent replay
		}
	}
	if err := c.WAL.Append(Record{Op: OpCommit, Segment: seg.Name, Hash: seg.SHA256, Stage: "commit"}); err != nil {
		return err
	}
	next := &Manifest{
		Version:  CurrentManifestVersion,
		Segments: append(append([]SegmentCommit(nil), c.Manifest.Segments...), seg),
		Stages:   copyStages(c.Manifest.Stages),
	}
	if next.Stages == nil {
		next.Stages = map[string]string{}
	}
	next.Stages["commit"] = "done"
	if err := SaveManifest(c.Dir, next); err != nil {
		return err
	}
	c.Manifest = next
	return nil
}

// Rollback marks a failed stage and rewrites the manifest without any
// segment that was staged but never committed. Committed segments are
// untouched, which is what makes "committed hashes never change" true.
func (c *CommitRun) Rollback(stage, reason string) error {
	if err := c.WAL.Append(Record{Op: OpRollback, Stage: stage, Detail: reason}); err != nil {
		return err
	}
	next := &Manifest{
		Version:  CurrentManifestVersion,
		Segments: append([]SegmentCommit(nil), c.Manifest.Segments...),
		Stages:   copyStages(c.Manifest.Stages),
	}
	if next.Stages == nil {
		next.Stages = map[string]string{}
	}
	next.Stages[stage] = "rolled-back"
	if err := SaveManifest(c.Dir, next); err != nil {
		return err
	}
	c.Manifest = next
	return nil
}

// Check records an invariant check result for the audit trail.
func (c *CommitRun) Check(stage, detail string) error {
	return c.WAL.Append(Record{Op: OpCheck, Stage: stage, Detail: detail})
}
