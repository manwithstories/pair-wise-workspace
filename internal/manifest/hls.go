package manifest

import (
	"bufio"
	"io"
	"strconv"
	"strings"

	"archive/internal/model"
)

// ParseHLS reads a local HLS media playlist (the playlist file itself is
// supplied as r; base is its containing directory, used to confine segment
// references). It supports the subset needed for archive recovery:
// EXTINF durations, explicit or implicit sequence numbers and
// EXT-X-DISCONTINUITY markers. Master playlists and remote media are
// rejected.
func ParseHLS(r io.Reader, base string, log *model.FaultLog) (*Timeline, error) {
	b := newBuilder(FormatHLS, base, log)
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	seq := -1 // -1 means "not declared"; implicit numbering starts at 0
	nextImplicit := 0
	pendingDur := 0.0
	haveDur := false
	discont := false
	lineNo := 0

	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#EXT-X-VERSION") {
			continue
		}
		switch {
		case strings.HasPrefix(line, "#EXT-X-MEDIA-SEQUENCE:"):
			if nextImplicit > 0 {
				return nil, model.InvariantError(model.PhaseManifest,
					"line %d: MEDIA-SEQUENCE after first segment", lineNo)
			}
			v, err := strconv.Atoi(strings.TrimPrefix(line, "#EXT-X-MEDIA-SEQUENCE:"))
			if err != nil || v < 0 {
				return nil, model.InvariantError(model.PhaseManifest, "line %d: bad media sequence", lineNo)
			}
			seq = v
		case strings.HasPrefix(line, "#EXT-X-DISCONTINUITY"):
			discont = true
		case strings.HasPrefix(line, "#EXTINF:"):
			raw := strings.TrimPrefix(line, "#EXTINF:")
			if comma := strings.IndexByte(raw, ','); comma >= 0 {
				raw = raw[:comma]
			}
			dur, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
			if err != nil {
				return nil, model.InvariantError(model.PhaseManifest, "line %d: bad EXTINF", lineNo)
			}
			pendingDur, haveDur = dur, true
		case strings.HasPrefix(line, "#EXT-X-STREAM-INF"):
			return nil, model.InvariantError(model.PhaseManifest, "master playlists are not supported")
		case strings.HasPrefix(line, "#"):
			// Unknown tags are ignored; archives only need the timing core.
			continue
		default:
			// URI line: commit one segment.
			if !haveDur {
				return nil, model.InvariantError(model.PhaseManifest, "line %d: segment without EXTINF", lineNo)
			}
			useSeq := nextImplicit
			if seq >= 0 {
				useSeq = seq + nextImplicit
			}
			if err := b.add(useSeq, line, pendingDur, discont); err != nil {
				return nil, err
			}
			nextImplicit++
			pendingDur, haveDur, discont = 0, false, false
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, model.InvariantError(model.PhaseManifest, "read error: %v", err)
	}
	if len(b.tl.Segments) == 0 {
		return nil, model.InvariantError(model.PhaseManifest, "playlist contains no segments")
	}
	return b.tl, nil
}
