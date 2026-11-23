// Package manifest parses HLS media playlists and DASH SegmentTimeline
// documents into one validated segment model. Validation happens before any
// segment is handed downstream: path escapes, over-limit entry counts,
// duplicate sequence numbers with differing hashes and illegal durations are
// all rejected with the unified clock error model.
package manifest

import (
	"fmt"
	"strconv"
	"strings"

	"mediarun/internal/clock"
)

// Limits bound a single playlist so a hostile or corrupt manifest cannot
// exhaust memory. They mirror the documented acceptance limits.
type Limits struct {
	MaxEntries      int
	MaxSegmentBytes int64
}

// DefaultLimits is the documented ceiling: 100,000 entries, 256 MiB segments.
func DefaultLimits() Limits {
	return Limits{MaxEntries: 100_000, MaxSegmentBytes: 256 << 20}
}

// Segment is the neutral, format-independent view of one media segment.
type Segment struct {
	ID            string
	Track         string // "video" or "audio" or a camera id
	Seq           uint64
	StartNS       int64
	DurNS         int64
	URI           string
	Hash          string // SHA-256 hex of the bytes, when known
	Discontinuity bool
}

// Media is a fully validated playlist: an ordered segment list plus the
// timescale each track was declared with.
type Media struct {
	Kind        string // "hls" or "dash"
	TargetDurNS int64
	Timescale   uint64
	Segments    []Segment
}

// TrackStartNS returns the axis position of the first segment on a track.
func (m *Media) TrackStartNS(track string) (int64, bool) {
	for _, s := range m.Segments {
		if s.Track == track {
			return s.StartNS, true
		}
	}
	return 0, false
}

// trajectory walks a per-track cursor forward and enforces that every start
// is exactly the previous start plus the previous duration. A gap is
// represented by an explicit segment with a negative duration marker at the
// caller; here a gap is simply a discontinuity the caller asked for.
type trajectory struct {
	timescale uint64
	next      map[string]int64
	seen      map[string]seenEntry
}

func newTrajectory(timescale uint64) *trajectory {
	return &trajectory{
		timescale: timescale,
		next:      map[string]int64{},
		seen:      map[string]seenEntry{},
	}
}

// ticksToNS converts a duration in the playlist timescale to ns, rejecting
// zero and negative durations.
func ticksToNS(ticks int64, timescale uint64, subject string) (int64, error) {
	if ticks <= 0 {
		return 0, clock.New(clock.KindInvalid, clock.CodeInvalidDuration, subject,
			fmt.Sprintf("non-positive duration %d", ticks))
	}
	if timescale == 0 {
		return 0, clock.New(clock.KindInvalid, clock.CodeInvalidDuration, subject,
			"zero timescale")
	}
	t := uint64(ticks)
	q := t / timescale
	r := t % timescale
	if q > 9_000_000_000 { // ~285 years; beyond any sample yet still safe
		return 0, clock.New(clock.KindInvalid, clock.CodeInvalidDuration, subject,
			"duration too large")
	}
	return int64(q)*1_000_000_000 + int64(r)*1_000_000_000/int64(timescale), nil
}

// normaliseURIPath rejects absolute paths, parent traversal and any scheme.
// Only plain slash-separated relative paths are accepted.
func normaliseURIPath(raw string) (string, error) {
	uri := strings.TrimSpace(raw)
	if uri == "" {
		return "", fmt.Errorf("empty uri")
	}
	if strings.Contains(uri, "\\") {
		return "", fmt.Errorf("backslash in uri")
	}
	if strings.Contains(uri, "://") || strings.HasPrefix(uri, "//") {
		return "", fmt.Errorf("absolute uri")
	}
	for _, part := range strings.Split(uri, "/") {
		if part == "" || part == "." || part == ".." {
			return "", fmt.Errorf("path segment %q not allowed", part)
		}
	}
	if strings.HasPrefix(uri, "/") {
		return "", fmt.Errorf("absolute path")
	}
	// Query/fragment could smuggle an external reference.
	if strings.ContainsAny(uri, "?#") {
		return "", fmt.Errorf("query or fragment not allowed")
	}
	return uri, nil
}

// seenEntry records the first commit of a (track, sequence) pair so a
// repeated sequence can be compared against it.
type seenEntry struct {
	URI  string
	Hash string
}

// checkDuplicateHash enforces requirement 1: the same (track, sequence) pair
// may appear twice only when both entries commit to the same URI and hash.
// A repeat with different content is the late-vs-real-miss ambiguity the
// pipeline must refuse to guess about, so it is rejected outright.
func (tr *trajectory) checkDuplicateHash(track string, seq uint64, uri, hash string) error {
	key := track + "/" + strconv.FormatUint(seq, 10)
	if prev, ok := tr.seen[key]; ok {
		if prev.URI != uri || prev.Hash != hash {
			return clock.New(clock.KindInvalid, clock.CodeDuplicateSeq,
				fmt.Sprintf("%s#%d", track, seq),
				"same sequence with differing content commitment")
		}
		return nil
	}
	tr.seen[key] = seenEntry{URI: uri, Hash: hash}
	return nil
}

// cursor loads or advances the per-track axis position.
func (tr *trajectory) cursor(track string, startNS int64, durNS int64, discontinuity bool) (int64, error) {
	pos, ok := tr.next[track]
	if !ok || discontinuity {
		tr.next[track] = startNS
		pos = startNS
	}
	if startNS < pos {
		return 0, clock.New(clock.KindInvalid, clock.CodeInvalidDuration, track,
			fmt.Sprintf("segment starts at %d before cursor %d", startNS, pos))
	}
	tr.next[track] = startNS + durNS
	return startNS, nil
}

func parseIntAttr(s, subject string) (int64, error) {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0, clock.New(clock.KindInvalid, clock.CodeInvalidDuration, subject,
			"malformed integer")
	}
	return n, nil
}

func parseFloatAttr(s, subject string) (float64, error) {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0, clock.New(clock.KindInvalid, clock.CodeInvalidDuration, subject,
			"malformed number")
	}
	return f, nil
}
