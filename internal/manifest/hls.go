package manifest

import (
	"bufio"
	"bytes"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/clearliu/fieldcut/internal/clock"
)

// ParseHLS parses an HLS media playlist (RFC 8216 subset) into ordered
// SegmentEvents. trackID is assigned to every emitted event because a
// single media playlist describes one rendition. The parser is offline:
// only relative local segment URIs are accepted.
func ParseHLS(data []byte, trackID int, opts Options) ([]SegmentEvent, error) {
	opts = opts.withDefaults()
	if trackID < 0 || trackID >= opts.MaxTracks {
		return nil, fmt.Errorf("%w: track id %d out of range", clock.ErrInvalidArgument, trackID)
	}
	r := bufio.NewScanner(bytes.NewReader(data))
	r.Buffer(make([]byte, 0, 64*1024), opts.MaxURIBytes*4)

	var (
		events        []SegmentEvent
		mediaSeq      = 0
		haveSeq       bool
		pendingInf    bool
		pendingDur    clock.Ticks
		disc          bool
		pendingRange  *ByteRange
		nextRangeOff  int64
		headerOK      bool
		cursor        int
		seen          = map[int]string{}
	)
	for r.Scan() {
		line := strings.TrimSpace(r.Text())
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "#") {
			// URI line.
			uri, err := safePath(line)
			if err != nil {
				return nil, err
			}
			seq := mediaSeq + cursor
			if prev, ok := seen[seq]; ok && prev != uri {
				return nil, fmt.Errorf("%w: seq %d (%q vs %q)", clock.ErrDuplicateSeq, seq, prev, uri)
			}
			seen[seq] = uri
			ev := SegmentEvent{
				Protocol:      ProtocolHLS,
				Kind:          KindMedia,
				TrackID:       trackID,
				Sequence:      seq,
				URI:           uri,
				Duration:      pendingDur,
				Start:         timelineStart(events),
				Discontinuity: disc,
				ByteRange:     pendingRange,
			}
			events = append(events, ev)
			cursor++
			pendingInf, pendingDur, disc, pendingRange = false, 0, false, nil
			if len(events) > opts.MaxSegments {
				return nil, fmt.Errorf("%w: > %d segments", clock.ErrManifestTooLarge, opts.MaxSegments)
			}
			continue
		}
		tag, param, _ := strings.Cut(line, ":")
		switch tag {
		case "#EXTM3U":
			headerOK = true
		case "#EXT-X-MEDIA-SEQUENCE":
			if haveSeq {
				return nil, fmt.Errorf("%w: duplicate media sequence tag", clock.ErrInvalidArgument)
			}
			v, err := strconv.Atoi(strings.TrimSpace(param))
			if err != nil || v < 0 {
				return nil, fmt.Errorf("%w: bad media sequence %q", clock.ErrInvalidArgument, param)
			}
			mediaSeq, haveSeq = v, true
		case "#EXTINF":
			dur, err := parseEXTINF(param)
			if err != nil {
				return nil, err
			}
			pendingInf, pendingDur = true, dur
		case "#EXT-X-DISCONTINUITY":
			disc = true
		case "#EXT-X-BYTERANGE":
			br, err := parseByteRange(param, nextRangeOff)
			if err != nil {
				return nil, err
			}
			pendingRange = &br
			nextRangeOff = br.End()
		case "#EXT-X-MAP":
			ev, err := parseMap(param, trackID, opts.MaxURIBytes)
			if err != nil {
				return nil, err
			}
			events = append(events, ev)
		case "#EXT-X-KEY":
			if !strings.Contains(param, "METHOD=NONE") && !strings.Contains(param, `METHOD="NONE"`) {
				return nil, fmt.Errorf("%w: encrypted media playlists are not supported", clock.ErrInvalidArgument)
			}
		default:
			// Unknown tags are intentionally ignored for forward compat.
		}
	}
	if err := r.Err(); err != nil {
		return nil, fmt.Errorf("%w: %v", clock.ErrManifestTooLarge, err)
	}
	if !headerOK {
		return nil, fmt.Errorf("%w: missing #EXTM3U header", clock.ErrInvalidArgument)
	}
	// Every media URI must have been preceded by EXTINF.
	if pendingInf {
		return nil, fmt.Errorf("%w: dangling EXTINF without segment", clock.ErrInvalidArgument)
	}
	if err := CheckSequenceConflicts(events); err != nil {
		return nil, err
	}
	return events, nil
}

func timelineStart(events []SegmentEvent) clock.Ticks {
	var t clock.Ticks
	for _, e := range events {
		if e.Kind == KindMedia {
			t += e.Duration
		}
	}
	return t
}

func parseEXTINF(param string) (clock.Ticks, error) {
	durField, _, _ := strings.Cut(param, ",")
	secs, err := strconv.ParseFloat(strings.TrimSpace(durField), 64)
	if err != nil || secs < 0 || math.IsNaN(secs) || math.IsInf(secs, 0) {
		return 0, fmt.Errorf("%w: bad EXTINF %q", clock.ErrInvalidArgument, param)
	}
	return clock.Ticks(math.Round(secs * float64(clock.Rate))), nil
}

func parseByteRange(param string, cont int64) (ByteRange, error) {
	parts := strings.SplitN(strings.TrimSpace(param), "@", 2)
	n, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || n <= 0 {
		return ByteRange{}, fmt.Errorf("%w: bad byte range %q", clock.ErrInvalidArgument, param)
	}
	off := cont
	if len(parts) == 2 {
		off, err = strconv.ParseInt(parts[1], 10, 64)
		if err != nil || off < 0 {
			return ByteRange{}, fmt.Errorf("%w: bad byte range offset %q", clock.ErrInvalidArgument, param)
		}
	}
	return ByteRange{Start: off, Length: n}, nil
}

func parseMap(param string, trackID, maxURIBytes int) (SegmentEvent, error) {
	var uri string
	for _, attr := range splitAttrs(param) {
		k, v, ok := strings.Cut(attr, "=")
		if ok && strings.TrimSpace(k) == "URI" {
			uri = strings.Trim(v, `"`)
		}
	}
	if len(uri) > maxURIBytes {
		return SegmentEvent{}, fmt.Errorf("%w: map uri too long", clock.ErrInvalidArgument)
	}
	safe, err := safePath(uri)
	if err != nil {
		return SegmentEvent{}, err
	}
	return SegmentEvent{Protocol: ProtocolHLS, Kind: KindInit, TrackID: trackID, URI: safe}, nil
}

// splitAttrs splits a comma separated HLS attribute list without breaking
// quoted values.
func splitAttrs(s string) []string {
	var out []string
	var buf strings.Builder
	inQuote := false
	for _, r := range s {
		switch {
		case r == '"':
			inQuote = !inQuote
			buf.WriteRune(r)
		case r == ',' && !inQuote:
			out = append(out, strings.TrimSpace(buf.String()))
			buf.Reset()
		default:
			buf.WriteRune(r)
		}
	}
	if buf.Len() > 0 {
		out = append(out, strings.TrimSpace(buf.String()))
	}
	return out
}
