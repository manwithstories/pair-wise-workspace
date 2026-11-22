package index

import (
	"fmt"
	"sort"

	"github.com/clearliu/fieldcut/internal/clock"
)

// SeekResult reports what a lookup found.
type SeekResult struct {
	Found  bool
	Entry  Entry
	// Miss is true when no entry covers the requested position. Misses
	// are counted by SeekTable and feed the index_seek_misses metric.
	Miss   bool
	// Degraded is true when the only matching entry is untrusted.
	Degraded bool
}

// QuarantineRecord describes a fragment refused during indexing, e.g. a
// truncated box. Quarantined fragments never produce entries and never
// affect the digest, but stay reportable for the operator.
type QuarantineRecord struct {
	TrackID  int
	Sequence int
	Reason   string
}

// SeekTable wraps a verified index and provides deterministic lookups
// plus a quarantine list for isolated corrupt fragments.
type SeekTable struct {
	idx        *Index
	misses     int
	lookups    int
	quarantine []QuarantineRecord
}

// NewSeekTable validates the index and wraps it for seeking.
func NewSeekTable(idx *Index) (*SeekTable, error) {
	if err := Verify(idx); err != nil {
		return nil, err
	}
	return &SeekTable{idx: idx}, nil
}

// SeekAt returns the latest entry whose Start is <= t on the requested
// track (TrackID < 0 means any track, tie-broken by the canonical total
// order). An untrusted match is returned with Degraded rather than hidden.
func (s *SeekTable) SeekAt(t clock.Ticks, trackID int) SeekResult {
	s.lookups++
	// Entries are globally sorted; a binary search finds the first entry
	// strictly after t, then we walk back to the best eligible match.
	up := sort.Search(len(s.idx.Entries), func(i int) bool {
		return s.idx.Entries[i].Start > t
	})
	for i := up - 1; i >= 0; i-- {
		e := s.idx.Entries[i]
		if trackID >= 0 && e.TrackID != trackID {
			continue
		}
		return SeekResult{Found: true, Entry: e, Degraded: e.Untrusted}
	}
	s.misses++
	return SeekResult{Miss: true}
}

// SeekExact looks for an entry starting at exactly t on a track. Several
// tracks can share the same start, so the scan continues across all
// entries at t rather than stopping at the first one.
func (s *SeekTable) SeekExact(t clock.Ticks, trackID int) SeekResult {
	s.lookups++
	i := sort.Search(len(s.idx.Entries), func(i int) bool {
		return s.idx.Entries[i].Start >= t
	})
	for i < len(s.idx.Entries) && s.idx.Entries[i].Start == t {
		e := s.idx.Entries[i]
		if trackID < 0 || e.TrackID == trackID {
			return SeekResult{Found: true, Entry: e, Degraded: e.Untrusted}
		}
		i++
	}
	s.misses++
	return SeekResult{Miss: true}
}

// Quarantine records an isolated corrupt fragment so it is reported
// without aborting the whole indexing run.
func (s *SeekTable) Quarantine(trackID, seq int, reason string) {
	s.quarantine = append(s.quarantine, QuarantineRecord{
		TrackID: trackID, Sequence: seq, Reason: reason,
	})
}

// QuarantineList returns a copy of the isolated fragment records.
func (s *SeekTable) QuarantineList() []QuarantineRecord {
	out := make([]QuarantineRecord, len(s.quarantine))
	copy(out, s.quarantine)
	return out
}

// Stats returns lookup counters used by the demo metrics.
func (s *SeekTable) Stats() (lookups, misses int) {
	return s.lookups, s.misses
}

// Index returns the underlying verified index.
func (s *SeekTable) Index() *Index { return s.idx }

// RerunDigest is a convenience that rebuilds the whole index from the
// same entry set and confirms the digest is stable across two independent
// builds. A mismatch is an invariant failure (exit code 1 territory).
func RerunDigest(entries []Entry) (string, error) {
	build := func() (string, error) {
		b := NewBuilder()
		for _, e := range entries {
			if err := b.Add(e); err != nil {
				return "", err
			}
		}
		idx, err := b.Build()
		if err != nil {
			return "", err
		}
		return idx.Digest()
	}
	first, err := build()
	if err != nil {
		return "", err
	}
	second, err := build()
	if err != nil {
		return "", err
	}
	if first != second {
		return "", fmt.Errorf("%w: digest drift %s != %s", clock.ErrCheckpoint, first, second)
	}
	return first, nil
}
