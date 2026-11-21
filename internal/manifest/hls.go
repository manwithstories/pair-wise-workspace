package manifest

import (
	"fmt"
	"strconv"
	"strings"

	"mediascan/internal/model"
)

// HLSTimescale is the rational clock used for every HLS EXTINF value; it is
// nanosecond-precise so decimal durations never silently round.
const HLSTimescale uint32 = 1_000_000_000

// ParseHLS parses a media playlist into the shared timeline model. Only media
// playlists are accepted: master playlists are rejected as unsupported so a
// caller can never accidentally treat a variant list as segments.
func ParseHLS(r *Resolver, relPath string, data []byte) (*Timeline, error) {
	res := &Resolver{Root: r.Root}
	tl := &Timeline{
		Source:    relPath,
		Format:    "hls",
		Root:      r.Root,
		Timescale: HLSTimescale,
	}
	stream := Stream{ID: "hls", Kind: "unknown", Timescale: HLSTimescale}
	var (
		cursor      = model.Zero
		pendingDur  *model.Rational
		pendingName string
		pendingByte [2]int64
		haveRange   bool
		sawExtinf   bool
		sawVariant  bool
	)
	seen := map[string]bool{}

	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	for lineNo, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#EXT-X-STREAM-INF") || strings.HasPrefix(line, "#EXT-X-I-FRAME-STREAM-INF") {
			sawVariant = true
			continue
		}
		if strings.HasPrefix(line, "#EXT-X-BYTERANGE:") {
			start, length, err := parseHLByteRange(strings.TrimSpace(strings.TrimPrefix(line, "#EXT-X-BYTERANGE:")))
			if err != nil {
				return nil, err
			}
			pendingByte = [2]int64{start, start + length - 1}
			haveRange = true
			continue
		}
		if strings.HasPrefix(line, "#EXTINF:") {
			value := strings.TrimPrefix(line, "#EXTINF:")
			if i := strings.IndexByte(value, ','); i >= 0 {
				value = value[:i]
			}
			dur, err := model.FromString(strings.TrimSpace(value))
			if err != nil {
				return nil, wrapLine(relPath, lineNo, err)
			}
			if dur.Sign() < 0 {
				return nil, wrapLine(relPath, lineNo, model.ErrNegativeTime)
			}
			pendingDur = &dur
			sawExtinf = true
			continue
		}
		if strings.HasPrefix(line, "#") {
			if strings.HasPrefix(line, "#EXT-X-MAP:URI=") {
				uri := strings.Trim(strings.TrimPrefix(line, "#EXT-X-MAP:URI="), `"`)
				if _, err := res.Resolve(uri); err != nil {
					return nil, wrapLine(relPath, lineNo, err)
				}
				stream.InitURI = uri
				tl.Referenced = append(tl.Referenced, uri)
			}
			continue
		}
		// A URI line. It must be preceded by EXTINF.
		pendingName = line
		if _, err := res.Resolve(pendingName); err != nil {
			return nil, wrapLine(relPath, lineNo, err)
		}
		if pendingDur == nil {
			// EXT-X-MAP style URI without EXTINF: not a media segment.
			continue
		}
		key := pendingName
		if haveRange {
			key = fmt.Sprintf("%s@%d", pendingName, pendingByte[0])
		}
		if seen[key] {
			return nil, wrapLine(relPath, lineNo, ErrUnsupported)
		}
		seen[key] = true
		seg := Segment{Name: baseName(pendingName), URI: pendingName, Start: cursor,
			Duration: *pendingDur, ByteStart: 0, ByteEnd: NoByteRange}
		if haveRange {
			seg.ByteStart, seg.ByteEnd = pendingByte[0], pendingByte[1]
		}
		validated, err := res.ValidateSegment(seg)
		if err != nil {
			return nil, wrapLine(relPath, lineNo, err)
		}
		stream.Segments = append(stream.Segments, validated)
		tl.Referenced = append(tl.Referenced, pendingName)
		cursor, err = cursor.Add(*pendingDur)
		if err != nil {
			return nil, wrapLine(relPath, lineNo, err)
		}
		if len(stream.Segments) > MaxSegments {
			return nil, ErrTooManyItems
		}
		pendingDur = nil
		haveRange = false
		pendingByte = [2]int64{}
	}
	if sawVariant && !sawExtinf {
		return nil, ErrUnsupported
	}
	if len(stream.Segments) == 0 {
		return nil, ErrBadManifest
	}
	stream.SegmentEnd = cursor
	tl.Duration = cursor
	tl.Streams = []Stream{stream}
	return tl, nil
}

// parseHLByteRange parses "length[@offset]". HLS offsets default to the end of
// the previous sub-range of the same resource; because the fixtures always
// carry explicit offsets and consecutive sub-ranges share a name, the parser
// requires an explicit offset for safety and reports a clear error otherwise.
func parseHLByteRange(s string) (start, length int64, err error) {
	lenStr, offStr, hasOff := strings.Cut(s, "@")
	length, err = strconv.ParseInt(strings.TrimSpace(lenStr), 10, 64)
	if err != nil || length <= 0 {
		return 0, 0, ErrBadManifest
	}
	if !hasOff {
		return 0, 0, ErrUnsupported
	}
	start, err = strconv.ParseInt(strings.TrimSpace(offStr), 10, 64)
	if err != nil || start < 0 {
		return 0, 0, ErrBadManifest
	}
	return start, length, nil
}

func baseName(uri string) string {
	if i := strings.LastIndexAny(uri, `/\`); i >= 0 {
		return uri[i+1:]
	}
	return uri
}
