package pipeline

import (
	"time"

	"relayaudit/internal/checkpoint"
	"relayaudit/internal/fault"
	"relayaudit/internal/transform"
	"relayaudit/internal/wire"
)

// commitCursor tracks the bytes already persisted to staging, so a checkpoint
// only appends the delta since the previous commit.
type commitCursor struct {
	staged int
}

// commit validates and persists the current prefix plus its cursor. Only the
// bytes appended since the last commit are written to the staging file; the
// state records the digest of the whole prefix.
func commit(store *checkpoint.Store, enc *transform.Encoder, sourceHash string, next int, out *Outcome, cur *commitCursor) error {
	stream := enc.Bytes()
	if cur == nil {
		cur = &commitCursor{}
	}
	if cur.staged > len(stream) {
		return fault.New(fault.Checkpoint, 0, "checkpoint cursor %d exceeds %d emitted bytes", cur.staged, len(stream))
	}
	st := checkpoint.State{
		SourceSHA256: sourceHash,
		NextIndex:    next,
		Kept:         enc.Kept(),
		Dropped:      droppedCount(out.Scan, enc.Kept()),
		EmittedBytes: int64(len(stream)),
		PrefixSHA256: enc.Digest(),
	}
	if cur.staged == len(stream) && st.EmittedBytes > 0 {
		// Nothing new to append: keep the persisted stamp unchanged.
		return store.Save(st)
	}
	if err := store.CommitAppend(st, stream, stream[cur.staged:]); err != nil {
		return err
	}
	cur.staged = len(stream)
	return nil
}

// droppedCount derives the isolated record count from the declared total.
func droppedCount(scan *wire.ScanResult, kept int) int {
	if scan == nil {
		return 0
	}
	d := int(scan.Header.RecordCount) - kept
	if d < 0 {
		return 0
	}
	return d
}

func finish(o *Outcome) *Outcome { return o }

func sinceMillis(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	ms := time.Since(t).Milliseconds()
	if ms < 0 {
		return 0
	}
	return ms
}
