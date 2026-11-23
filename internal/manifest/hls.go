package manifest

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"math"
	"strings"

	"mediarun/internal/clock"
)

// ParseHLS parses an HLS media playlist. It accepts both the plain
// duration form (#EXTINF) and the byte-range form, and validates every URI
// against the path-escaping rules before returning.
func ParseHLS(r io.Reader, limits Limits) (*Media, error) {
	return ParseHLSForTrack(r, limits, "video")
}

// ParseHLSForTrack parses a media playlist and labels its segments with the
// given track name. HLS media playlists carry no track identity, so the
// caller (one playlist per camera or per rendition) supplies it.
func ParseHLSForTrack(r io.Reader, limits Limits, track string) (*Media, error) {
	if track == "" {
		track = "video"
	}
	if limits.MaxEntries <= 0 {
		limits = DefaultLimits()
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)

	var (
		headerSeen  bool
		mediaSeq    uint64
		seqSet      bool
		targetNS    int64
		pendingDur  int64
		pendingHash string
		pendingDisc bool
		posNS       int64
		entries     int
	)
	media := &Media{Kind: "hls"}
	// HLS declares durations in seconds; we represent them in ms ticks and
	// convert to the unified axis with timescale 1000.
	tr := newTrajectory(1000)

	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if !headerSeen {
			if line != "#EXTM3U" {
				return nil, clock.New(clock.KindInvalid, clock.CodeUnsupported, "",
					"missing #EXTM3U header")
			}
			headerSeen = true
			continue
		}
		switch {
		case strings.HasPrefix(line, "#EXT-X-VERSION:"):
			v, err := parseIntAttr(line[len("#EXT-X-VERSION:"):], "version")
			if err != nil {
				return nil, err
			}
			if v < 2 || v > 20 {
				return nil, clock.New(clock.KindInvalid, clock.CodeUnsupported, "",
					"unsupported playlist version")
			}
		case strings.HasPrefix(line, "#EXT-X-TARGETDURATION:"):
			v, err := parseFloatAttr(line[len("#EXT-X-TARGETDURATION:"):], "target")
			if err != nil {
				return nil, err
			}
			if v <= 0 || v > 3600 {
				return nil, clock.New(clock.KindInvalid, clock.CodeInvalidDuration, "",
					"target duration out of range")
			}
			ms := int64(v * 1000)
			ns, err := ticksToNS(ms, 1000, "target")
			if err != nil {
				return nil, err
			}
			targetNS = ns
		case strings.HasPrefix(line, "#EXT-X-MEDIA-SEQUENCE:"):
			v, err := parseIntAttr(line[len("#EXT-X-MEDIA-SEQUENCE:"):], "seq")
			if err != nil {
				return nil, err
			}
			if v < 0 {
				return nil, clock.New(clock.KindInvalid, clock.CodeInvalidDuration, "",
					"negative media sequence")
			}
			mediaSeq = uint64(v)
			seqSet = true
		case strings.HasPrefix(line, "#EXT-X-MEDIA-HASH:"):
			h := strings.TrimSpace(line[len("#EXT-X-MEDIA-HASH:"):])
			if len(h) != 64 || !isHex(h) {
				return nil, clock.New(clock.KindInvalid, clock.CodeDuplicateSeq, "",
					"malformed segment hash")
			}
			pendingHash = strings.ToLower(h)
		case strings.HasPrefix(line, "#EXT-X-DISCONTINUITY"):
			pendingDisc = true
		case strings.HasPrefix(line, "#EXTINF:"):
			head := line[len("#EXTINF:"):]
			if i := strings.IndexByte(head, ','); i >= 0 {
				head = head[:i]
			}
			v, err := parseFloatAttr(head, "extinf")
			if err != nil {
				return nil, err
			}
			if v <= 0 || v > 3600 {
				return nil, clock.New(clock.KindInvalid, clock.CodeInvalidDuration, "",
					"EXTINF out of range")
			}
			ms := int64(math.Ceil(v * 1000))
			ns, err := ticksToNS(ms, 1000, "extinf")
			if err != nil {
				return nil, err
			}
			pendingDur = ns
		case strings.HasPrefix(line, "#EXT-X-STREAM-INF"):
			return nil, clock.New(clock.KindInvalid, clock.CodeUnsupported, "",
				"master playlists are not accepted; provide a media playlist")
		case strings.HasPrefix(line, "#"):
			continue
		default:
			// URI line.
			uri, err := normaliseURIPath(line)
			if err != nil {
				return nil, clock.New(clock.KindInvalid, clock.CodePathEscape, line, err.Error())
			}
			if pendingDur == 0 {
				return nil, clock.New(clock.KindInvalid, clock.CodeInvalidDuration, uri,
					"URI without EXTINF duration")
			}
			entries++
			if entries > limits.MaxEntries {
				return nil, clock.New(clock.KindInvalid, clock.CodeEntryLimit, "",
					"playlist exceeds entry limit")
			}
			seq := mediaSeq
			mediaSeq++
			if err := tr.checkDuplicateHash(track, seq, uri, pendingHash); err != nil {
				return nil, err
			}
			start, err := tr.cursor(track, posNS, pendingDur, pendingDisc)
			if err != nil {
				return nil, err
			}
			posNS = start + pendingDur
			media.Segments = append(media.Segments, Segment{
				ID:            fmt.Sprintf("%s-%06d", track, seq),
				Track:         track,
				Seq:           seq,
				StartNS:       start,
				DurNS:         pendingDur,
				URI:           uri,
				Hash:          pendingHash,
				Discontinuity: pendingDisc,
			})
			pendingDur = 0
			pendingHash = ""
			pendingDisc = false
		}
	}
	if err := sc.Err(); err != nil {
		return nil, clock.Wrap(clock.KindInvalid, clock.CodeUnsupported, "", err)
	}
	if !headerSeen {
		return nil, clock.New(clock.KindInvalid, clock.CodeUnsupported, "", "empty playlist")
	}
	if !seqSet {
		mediaSeq = 0
	}
	if len(media.Segments) == 0 {
		return nil, clock.New(clock.KindInvalid, clock.CodeUnsupported, "", "no segments")
	}
	media.TargetDurNS = targetNS
	media.Timescale = 1000
	if media.TargetDurNS == 0 {
		// Derive from the longest declared segment.
		for _, s := range media.Segments {
			if s.DurNS > media.TargetDurNS {
				media.TargetDurNS = s.DurNS
			}
		}
	}
	return media, nil
}

func isHex(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// ParseHLSBytes is a convenience wrapper that additionally rejects a
// manifest whose own size exceeds the configured ceiling, so an oversized
// document is refused before it is tokenised.
func ParseHLSBytes(data []byte, limits Limits) (*Media, error) {
	if limits.MaxSegmentBytes > 0 && int64(len(data)) > limits.MaxSegmentBytes {
		return nil, clock.New(clock.KindInvalid, clock.CodeSizeLimit, "",
			"manifest exceeds size ceiling")
	}
	return ParseHLS(bytes.NewReader(data), limits)
}
