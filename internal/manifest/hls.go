// Package manifest parses HLS media playlists and DASH MPDs into
// timeline segments with dependencies.
package manifest

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"

	"mediaroute/internal/model"
)

// hlsTimescale is the clock used for HLS timelines (90 kHz, MPEG-TS).
const hlsTimescale = 90000

// ParseHLS parses an HLS media playlist. Every segment URI must be
// preceded by EXTINF; duplicate media-sequence numbers with conflicting
// URIs are rejected. Errors carry the 1-based playlist line.
func ParseHLS(r io.Reader, track string) ([]model.Segment, error) {
	sc := bufio.NewScanner(r)
	line := 0
	seqBase := int64(0)
	var segs []model.Segment
	var pendingDur float64
	haveDur := false
	seen := map[int64]string{} // sequence -> uri
	var start int64
	fail := func(format string, a ...any) error {
		return &model.MediaError{Code: model.ErrManifest, Module: "manifest",
			Track: track, Location: fmt.Sprintf("line %d", line),
			Msg: fmt.Sprintf(format, a...)}
	}
	for sc.Scan() {
		line++
		s := strings.TrimSpace(sc.Text())
		switch {
		case s == "" || s == "#EXTM3U":
		case strings.HasPrefix(s, "#EXT-X-MEDIA-SEQUENCE:"):
			v, err := strconv.ParseInt(strings.TrimPrefix(s, "#EXT-X-MEDIA-SEQUENCE:"), 10, 64)
			if err != nil {
				return nil, fail("bad media sequence: %v", err)
			}
			seqBase = v
		case strings.HasPrefix(s, "#EXTINF:"):
			v, err := strconv.ParseFloat(strings.TrimSuffix(strings.TrimPrefix(s, "#EXTINF:"), ","), 64)
			if err != nil || v <= 0 {
				return nil, fail("bad EXTINF duration %q", s)
			}
			pendingDur, haveDur = v, true
		case strings.HasPrefix(s, "#"):
			// other tags ignored
		default:
			if !haveDur {
				return nil, fail("segment %q missing EXTINF duration", s)
			}
			seq := seqBase + int64(len(segs))
			if prev, dup := seen[seq]; dup && prev != s {
				return nil, &model.MediaError{Code: model.ErrDuplicate, Module: "manifest",
					Track: track, Seq: seq, Location: fmt.Sprintf("line %d", line),
					Msg: fmt.Sprintf("sequence %d conflicts: %q vs %q", seq, prev, s)}
			}
			seen[seq] = s
			dur := int64(pendingDur * hlsTimescale)
			segs = append(segs, model.Segment{Index: len(segs), Track: track,
				Start: start, Duration: dur, URI: s})
			start += dur
			haveDur = false
		}
	}
	if err := sc.Err(); err != nil {
		return nil, &model.MediaError{Code: model.ErrManifest, Module: "manifest",
			Track: track, Msg: "read playlist", Cause: err}
	}
	if len(segs) == 0 {
		return nil, &model.MediaError{Code: model.ErrManifest, Module: "manifest",
			Track: track, Msg: "playlist has no segments"}
	}
	return segs, nil
}
