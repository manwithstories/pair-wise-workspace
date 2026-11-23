package media

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"hash/fnv"

	"medrelay/internal/timeline"
)

// Ref locates a fingerprinted window: its source segment and time range.
type Ref struct {
	Source  string
	StartMs int64
	EndMs   int64
}

// DupRecord maps a duplicate window back to its first occurrence.
type DupRecord struct {
	Window Ref
	DupOf  Ref
}

// DedupReport summarises deduplication of one segment.
type DupReport struct {
	Dups      []DupRecord
	Kept      []Ref
	Reasons   []string // why a window was kept despite suspicion
}

type windowEntry struct {
	ref  Ref
	hash uint64
	sum  [32]byte
}

// Deduper detects repeated content windows across segments. Duplicates are
// recorded with their source and time mapping; the original is always kept.
type Deduper struct {
	WindowMs int64
	seen     map[uint64][]windowEntry
}

func NewDeduper(windowMs int64) *Deduper {
	if windowMs <= 0 {
		windowMs = 1000
	}
	return &Deduper{WindowMs: windowMs, seen: map[uint64][]windowEntry{}}
}

// windowContent concatenates the data of frames starting inside the window.
func windowContent(frames []Frame, startMs, endMs int64) []byte {
	var b bytes.Buffer
	for _, f := range frames {
		s := timeline.ToMillis(f.PTS, f.TS)
		if s >= startMs && s < endMs {
			b.WriteByte(f.Kind)
			b.Write(f.Data)
		}
	}
	return b.Bytes()
}

// Process scans one segment's frames window by window. A window whose
// fingerprint matches a previous window with identical content is a
// duplicate; a fingerprint match with different content is a conflict and
// both copies are kept and reported. Windows shorter than WindowMs at the
// segment tail are kept with a reason.
func (d *Deduper) Process(source string, frames []Frame, segStartMs, segEndMs int64) DupReport {
	rep := DupReport{}
	for start := segStartMs; start < segEndMs; start += d.WindowMs {
		end := start + d.WindowMs
		ref := Ref{Source: source, StartMs: start, EndMs: end}
		if end > segEndMs {
			rep.Kept = append(rep.Kept, ref)
			rep.Reasons = append(rep.Reasons,
				fmt.Sprintf("%s [%dms,%dms): boundary window shorter than %dms, kept",
					source, start, segEndMs, d.WindowMs))
			break
		}
		content := windowContent(frames, start, end)
		h := fnv.New64a()
		h.Write(content)
		fh := h.Sum64()
		sum := sha256.Sum256(content)
		dup := false
		conflict := false
		for _, e := range d.seen[fh] {
			if e.sum == sum {
				rep.Dups = append(rep.Dups, DupRecord{Window: ref, DupOf: e.ref})
				dup = true
				break
			}
			conflict = true
		}
		if dup {
			continue
		}
		if conflict {
			rep.Kept = append(rep.Kept, ref)
			rep.Reasons = append(rep.Reasons,
				fmt.Sprintf("%s [%dms,%dms): fingerprint conflict, original kept",
					source, start, end))
			continue
		}
		d.seen[fh] = append(d.seen[fh], windowEntry{ref: ref, hash: fh, sum: sum})
		rep.Kept = append(rep.Kept, ref)
	}
	return rep
}
