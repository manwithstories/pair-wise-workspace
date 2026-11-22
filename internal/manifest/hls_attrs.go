package manifest

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

func lastSegment(segs []Segment) *Segment {
	if len(segs) == 0 {
		return nil
	}
	return &segs[len(segs)-1]
}

func offsetPtr(v int64) *int64 { return &v }

func cloneKey(k *Key) *Key {
	if k == nil {
		return nil
	}
	c := *k
	return &c
}

// parseAttrs parses HLS attribute lists: KEY=value,KEY="quoted,value".
func parseAttrs(s string) (map[string]string, error) {
	out := map[string]string{}
	for len(s) > 0 {
		s = strings.TrimLeft(s, " \t,")
		if s == "" {
			break
		}
		eq := strings.IndexByte(s, '=')
		if eq <= 0 {
			return nil, fmt.Errorf("missing '=' in %q", s)
		}
		key := strings.TrimSpace(s[:eq])
		s = s[eq+1:]
		var val string
		if len(s) > 0 && s[0] == '"' {
			end := strings.IndexByte(s[1:], '"')
			if end < 0 {
				return nil, fmt.Errorf("unterminated quote in attribute %s", key)
			}
			val = s[1 : 1+end]
			s = s[2+end:]
		} else {
			next := strings.IndexByte(s, ',')
			if next < 0 {
				val, s = s, ""
			} else {
				val, s = s[:next], s[next+1:]
			}
			val = strings.TrimSpace(val)
		}
		if key == "" {
			return nil, errors.New("empty attribute name")
		}
		out[key] = val
	}
	return out, nil
}

// parseRange parses "length[@offset]". prevEnd supplies the implicit offset.
func parseRange(s string, prevEnd *int64) (*Range, error) {
	s = strings.TrimSpace(s)
	lenText, offText, hasOff := strings.Cut(s, "@")
	n, err := strconv.ParseInt(strings.TrimSpace(lenText), 10, 64)
	if err != nil || n < 0 {
		return nil, fmt.Errorf("invalid byte range length %q", lenText)
	}
	r := &Range{Length: n}
	if hasOff {
		off, err := strconv.ParseInt(strings.TrimSpace(offText), 10, 64)
		if err != nil || off < 0 {
			return nil, fmt.Errorf("invalid byte range offset %q", offText)
		}
		r.Offset, r.HasOffset = off, true
	} else {
		if prevEnd == nil || *prevEnd < 0 {
			return nil, errors.New("implicit byte range offset without previous range")
		}
		r.Offset = *prevEnd
	}
	return r, nil
}
