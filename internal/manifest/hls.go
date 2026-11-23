// Package manifest parses HLS and DASH segment references into a unified
// segment list, rejecting malformed input at the first anomaly.
package manifest

import (
	"bufio"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"

	"medrelay/internal/timeline"
)

// Segment is one media segment referenced by a manifest.
type Segment struct {
	URI      string
	Seq      uint64
	Duration float64 // seconds
	StartMs  int64   // accumulated start on the unified clock
}

const (
	// MaxSegments bounds the segment count a manifest may declare.
	MaxSegments = 4096
	// MaxSegmentDurationS bounds a single segment duration.
	MaxSegmentDurationS = 3600.0
)

var seqRe = regexp.MustCompile(`^seg-(\d+)\.m4s$`)

// SeqFromURI extracts the sequence number encoded in a segment URI.
func SeqFromURI(uri string) (uint64, bool) {
	m := seqRe.FindStringSubmatch(uri)
	if m == nil {
		return 0, false
	}
	n, err := strconv.ParseUint(m[1], 10, 64)
	return n, err == nil
}

// ParseHLS parses a media playlist. It rejects duplicate sequence numbers,
// non-positive or oversized durations, EXTINF tags without a following URI
// and out-of-range accumulated timestamps, reporting the first anomaly.
func ParseHLS(r io.Reader) ([]Segment, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	var segs []Segment
	var pendingDur float64
	var haveDur bool
	var startMs int64
	seen := map[uint64]bool{}
	line := 0
	first := true
	for sc.Scan() {
		line++
		s := strings.TrimSpace(sc.Text())
		if s == "" {
			continue
		}
		if first {
			first = false
			if s != "#EXTM3U" {
				return nil, timeline.New(timeline.ErrMissingRef, "", 0, 0,
					fmt.Sprintf("line %d: playlist must start with #EXTM3U", line))
			}
			continue
		}
		if strings.HasPrefix(s, "#EXTINF:") {
			if haveDur {
				return nil, timeline.New(timeline.ErrMissingRef, "", startMs, startMs,
					fmt.Sprintf("line %d: EXTINF without following segment URI", line))
			}
			v := strings.TrimPrefix(s, "#EXTINF:")
			v = strings.TrimSuffix(strings.SplitN(v, ",", 2)[0], " ")
			d, err := strconv.ParseFloat(v, 64)
			if err != nil || d <= 0 || d > MaxSegmentDurationS {
				return nil, timeline.New(timeline.ErrInvalidDur, "", startMs, startMs,
					fmt.Sprintf("line %d: invalid EXTINF duration %q", line, v))
			}
			pendingDur, haveDur = d, true
			continue
		}
		if strings.HasPrefix(s, "#") {
			continue
		}
		// A URI line must follow an EXTINF.
		if !haveDur {
			return nil, timeline.New(timeline.ErrMissingRef, s, startMs, startMs,
				fmt.Sprintf("line %d: segment URI without preceding EXTINF", line))
		}
		seq, ok := SeqFromURI(s)
		if !ok {
			return nil, timeline.New(timeline.ErrMissingRef, s, startMs, startMs,
				fmt.Sprintf("line %d: segment URI %q does not match seg-<n>.m4s", line, s))
		}
		if seen[seq] {
			return nil, timeline.NewSeq(timeline.ErrDuplicateSeq, s, seq, startMs, startMs,
				fmt.Sprintf("line %d: duplicate sequence number", line))
		}
		seen[seq] = true
		endMs := startMs + int64(pendingDur*1000+0.5)
		if endMs > 24*3600*1000 {
			return nil, timeline.NewSeq(timeline.ErrTimestampRange, s, seq, startMs, endMs,
				fmt.Sprintf("line %d: accumulated timestamp exceeds 24h", line))
		}
		segs = append(segs, Segment{URI: s, Seq: seq, Duration: pendingDur, StartMs: startMs})
		startMs = endMs
		haveDur = false
		if len(segs) > MaxSegments {
			return nil, timeline.New(timeline.ErrLimit, s, 0, 0, "too many segments")
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(segs) == 0 {
		return nil, timeline.New(timeline.ErrMissingRef, "", 0, 0, "playlist contains no segments")
	}
	return segs, nil
}
