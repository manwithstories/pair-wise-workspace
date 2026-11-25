package manifest

import (
	"math/big"
	"path"
	"strconv"
	"strings"

	"mediad/internal/clock"
	"mediad/internal/fault"
)

// ParseHLS parses EXT-X-MAP, EXT-X-DISCONTINUITY and EXTINF entries.
// A missing MAP after discontinuity is retained as an empty InitURI so callers
// can propagate it into timeline and recovery validation.
func ParseHLS(file string, data []byte) (*Manifest, error) {
	text := string(data)
	lines := strings.SplitAfter(text, "\n")
	m := &Manifest{Kind: "hls", Path: file, Version: 1}
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "#EXTM3U" {
		return nil, fault.New(fault.InvalidManifest, file, 0, "missing #EXTM3U header")
	}

	var (
		offset        int64
		nextPeriod    int
		currentMap    string
		mapForPeriod  string
		haveMapPeriod bool
		extinfDur     *clock.Rational
		extinfOffset  int64
		index         int
	)

	for _, raw := range lines {
		line := strings.TrimRight(raw, "\r\n")
		lineOffset := offset
		offset += int64(len(raw))
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") && !strings.HasPrefix(trimmed, "#EXT") {
			continue
		}

		switch {
		case strings.HasPrefix(trimmed, "#EXT-X-VERSION:"):
			v, err := strconv.Atoi(strings.TrimPrefix(trimmed, "#EXT-X-VERSION:"))
			if err != nil || v <= 0 {
				return nil, fault.New(fault.UnknownVersion, file, lineOffset, "invalid HLS version")
			}
			if v > 7 {
				return nil, fault.New(fault.UnknownVersion, file, lineOffset, "unsupported HLS manifest version %d (adapter v1 supports <=7)", v)
			}
			m.Version = v

		case strings.HasPrefix(trimmed, "#EXT-X-MAP:"):
			attrs := parseAttrs(strings.TrimPrefix(trimmed, "#EXT-X-MAP:"))
			uri := attrs["URI"]
			if uri == "" {
				return nil, fault.New(fault.InitMissing, file, lineOffset, "EXT-X-MAP without URI")
			}
			currentMap = uri
			mapForPeriod = uri
			haveMapPeriod = true

		case trimmed == "#EXT-X-DISCONTINUITY":
			nextPeriod++
			// v1 requires a fresh EXT-X-MAP in every new fMP4 period.
			currentMap = ""
			haveMapPeriod = false

		case strings.HasPrefix(trimmed, "#EXTINF:"):
			body := strings.TrimPrefix(trimmed, "#EXTINF:")
			if i := strings.IndexByte(body, ','); i >= 0 {
				body = body[:i]
			}
			d, err := ParseDecimalRat(strings.TrimSpace(body))
			if err != nil {
				return nil, fault.New(fault.InvalidManifest, file, lineOffset, "invalid EXTINF duration: %v", err)
			}
			extinfDur = &d
			extinfOffset = lineOffset

		case !strings.HasPrefix(trimmed, "#"):
			if extinfDur == nil {
				return nil, fault.New(fault.InvalidManifest, file, lineOffset, "segment URI without EXTINF")
			}
			init := currentMap
			if nextPeriod == 0 {
				init = mapForPeriod
			} else if !haveMapPeriod {
				init = ""
			}
			m.Segments = append(m.Segments, Segment{
				Index: index, Period: nextPeriod, URI: trimmed, InitURI: init,
				Duration: *extinfDur, LineOffset: extinfOffset,
			})
			index++
			extinfDur = nil
		}
	}

	if len(m.Segments) == 0 {
		return nil, fault.New(fault.InvalidManifest, file, 0, "playlist contains no segments")
	}
	if err := assignHLSStarts(file, m.Segments); err != nil {
		return nil, err
	}
	return m, nil
}

func assignHLSStarts(file string, segments []Segment) error {
	periodStart := map[int]clock.Rational{}
	periodCursor := map[int]clock.Rational{}
	for i := range segments {
		s := &segments[i]
		if _, ok := periodStart[s.Period]; !ok {
			periodStart[s.Period] = clock.Zero
			periodCursor[s.Period] = clock.Zero
		}
		s.Start = periodCursor[s.Period]
		end, err := clock.Add(s.Start, s.Duration)
		if err != nil {
			return fault.New(fault.TimestampOverflow, file, s.LineOffset, "HLS duration overflow")
		}
		periodCursor[s.Period] = end
	}
	return nil
}

// ResolveURI resolves a playlist-relative URI.
func ResolveURI(playlistPath, uri string) string {
	if path.IsAbs(uri) {
		return uri
	}
	return path.Join(path.Dir(playlistPath), uri)
}

func parseAttrs(s string) map[string]string {
	out := map[string]string{}
	for _, part := range splitAttrs(s) {
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			continue
		}
		out[strings.TrimSpace(kv[0])] = strings.Trim(strings.TrimSpace(kv[1]), `"`)
	}
	return out
}

func splitAttrs(s string) []string {
	var parts []string
	var cur strings.Builder
	inQuote := false
	for _, r := range s {
		switch {
		case r == '"':
			inQuote = !inQuote
			cur.WriteRune(r)
		case r == ',' && !inQuote:
			parts = append(parts, cur.String())
			cur.Reset()
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		parts = append(parts, cur.String())
	}
	return parts
}

// ParseDecimalRat parses a decimal duration exactly as an integer rational.
func ParseDecimalRat(s string) (clock.Rational, error) {
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	parts := strings.SplitN(s, ".", 2)
	intPart := parts[0]
	if intPart == "" {
		intPart = "0"
	}
	digits := intPart
	den := big.NewInt(1)
	if len(parts) == 2 {
		digits += parts[1]
		for range parts[1] {
			den.Mul(den, big.NewInt(10))
		}
	}
	n, ok := new(big.Int).SetString(digits, 10)
	if !ok {
		return clock.Rational{}, strconv.ErrSyntax
	}
	if neg {
		n.Neg(n)
	}
	if !n.IsInt64() || !den.IsInt64() {
		return clock.Rational{}, fault.New(fault.TimestampOverflow, "", -1, "decimal duration exceeds int64")
	}
	return clock.New(n.Int64(), den.Int64())
}
