package txn

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
)

// Segment is one aligned pipeline segment presented for atomic commit.
type Segment struct {
	ID      uint64
	Start   int64 // media ticks, inclusive
	End     int64 // media ticks, exclusive
	Events  int   // number of aligned metadata events
	Payload []byte
	Digest  [32]byte // sha256 of Payload, supplied by the producer
}

// NewSegment builds a segment and computes its digest.
func NewSegment(id uint64, start, end int64, events int, payload []byte) Segment {
	return Segment{ID: id, Start: start, End: end, Events: events,
		Payload: payload, Digest: sha256.Sum256(payload)}
}

func (s Segment) marshal() []byte {
	buf := make([]byte, 0, 8+8+4+32+len(s.Payload))
	buf = binary.BigEndian.AppendUint64(buf, uint64(s.Start))
	buf = binary.BigEndian.AppendUint64(buf, uint64(s.End))
	buf = binary.BigEndian.AppendUint32(buf, uint32(s.Events))
	buf = append(buf, s.Digest[:]...)
	return append(buf, s.Payload...)
}

func unmarshalSegment(id uint64, buf []byte) (Segment, error) {
	if len(buf) < 52 {
		return Segment{}, fmt.Errorf("txn: segment %d record too short: %d", id, len(buf))
	}
	s := Segment{
		ID:      id,
		Start:   int64(binary.BigEndian.Uint64(buf[0:8])),
		End:     int64(binary.BigEndian.Uint64(buf[8:16])),
		Events:  int(binary.BigEndian.Uint32(buf[16:20])),
		Payload: append([]byte(nil), buf[52:]...),
	}
	copy(s.Digest[:], buf[20:52])
	return s, nil
}

// Committer validates segments and commits them atomically through the
// WAL: segment record first, then validation, then a commit marker.
// Failed validation or a torn log rolls the segment back; committed
// state is never rewritten.
type Committer struct {
	wal       *WAL
	committed []Segment
	rollbacks int
	horizon   int64 // end of the last committed segment
}

// NewCommitter replays recovered WAL records: segment records paired
// with a commit marker are committed; unpaired ones — and any torn tail
// discarded at open — are rolled back.
func NewCommitter(wal *WAL, recovered []Record, torn bool) *Committer {
	c := &Committer{wal: wal, horizon: -1}
	pending := make(map[uint64]Segment)
	for _, rec := range recovered {
		switch rec.Type {
		case RecSegment:
			if seg, err := unmarshalSegment(rec.SegmentID, rec.Payload); err == nil {
				pending[rec.SegmentID] = seg
			}
		case RecCommit:
			if seg, ok := pending[rec.SegmentID]; ok {
				c.committed = append(c.committed, seg)
				c.horizon = seg.End
				delete(pending, rec.SegmentID)
			}
		case RecAbort:
			delete(pending, rec.SegmentID)
		}
	}
	c.rollbacks = len(pending) // uncommitted tails rolled back on recovery
	if torn {
		c.rollbacks++
	}
	return c
}

// Submit validates and atomically commits one segment. On any validation
// failure the segment is aborted in the WAL and the last valid commit
// point is preserved.
func (c *Committer) Submit(seg Segment) error {
	if err := c.wal.Append(Record{Type: RecSegment, SegmentID: seg.ID, Payload: seg.marshal()}); err != nil {
		return err
	}
	if err := c.validate(seg); err != nil {
		_ = c.wal.Append(Record{Type: RecAbort, SegmentID: seg.ID})
		c.rollbacks++
		return err
	}
	if err := c.wal.Append(Record{Type: RecCommit, SegmentID: seg.ID}); err != nil {
		return err
	}
	c.committed = append(c.committed, seg)
	c.horizon = seg.End
	return nil
}

// Abort rolls back a segment whose transaction must be abandoned (e.g.
// on consumer timeout), preserving the last valid commit point.
func (c *Committer) Abort(id uint64) {
	_ = c.wal.Append(Record{Type: RecAbort, SegmentID: id})
	c.rollbacks++
}

func (c *Committer) validate(seg Segment) error {
	if got := sha256.Sum256(seg.Payload); got != seg.Digest {
		return fmt.Errorf("txn: segment %d: input digest mismatch", seg.ID)
	}
	if seg.End <= seg.Start {
		return fmt.Errorf("txn: segment %d: invalid time range [%d,%d)", seg.ID, seg.Start, seg.End)
	}
	if c.horizon >= 0 && seg.Start < c.horizon {
		return fmt.Errorf("txn: segment %d: start %d precedes committed horizon %d",
			seg.ID, seg.Start, c.horizon)
	}
	if seg.Events < 0 {
		return fmt.Errorf("txn: segment %d: negative event count %d", seg.ID, seg.Events)
	}
	return nil
}

// RollbackCount returns the number of rolled-back segments, including
// uncommitted tails discarded during recovery.
func (c *Committer) RollbackCount() int { return c.rollbacks }

// Committed returns the committed segments in commit order.
func (c *Committer) Committed() []Segment { return c.committed }

// Digest is the deterministic digest over all committed segments.
func (c *Committer) Digest() string {
	h := sha256.New()
	for _, seg := range c.committed {
		h.Write(seg.Digest[:])
	}
	return hex.EncodeToString(h.Sum(nil))
}
