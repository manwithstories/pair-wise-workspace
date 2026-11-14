// Package sdp parses the small subset of SDP the gateway needs: media
// sections, payload types, clock rates and control attributes, resolving
// both absolute and relative control URLs into full track URLs.
package sdp

import (
	"fmt"
	"strconv"
	"strings"
)

// Track describes one m= section of the session description.
type Track struct {
	Index       int
	Media       string
	PayloadType uint8
	ClockRate   int
	Control     string // raw a=control value, may be relative
	URL         string // fully resolved track URL
}

// Session is the parsed SDP body of a DESCRIBE response.
type Session struct {
	Control string // session-level a=control, may be empty
	Tracks  []*Track
}

// TrackByPayloadType returns the track carrying pt, or nil.
func (s *Session) TrackByPayloadType(pt uint8) *Track {
	for _, t := range s.Tracks {
		if t.PayloadType == pt {
			return t
		}
	}
	return nil
}

// Parse decodes an SDP body. baseURL is the URL used in the DESCRIBE request
// and is the fallback when the session-level a=control is absent.
func Parse(body []byte, baseURL string) (*Session, error) {
	s := &Session{}
	var cur *Track
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		switch {
		case strings.HasPrefix(line, "m="):
			t, err := parseMedia(line)
			if err != nil {
				return nil, err
			}
			t.Index = len(s.Tracks)
			s.Tracks = append(s.Tracks, t)
			cur = t
		case strings.HasPrefix(line, "a=control:"):
			v := strings.TrimPrefix(line, "a=control:")
			if cur == nil {
				s.Control = v
			} else {
				cur.Control = v
			}
		case strings.HasPrefix(line, "a=rtpmap:"):
			if cur != nil {
				if err := parseRTPMap(line, cur); err != nil {
					return nil, err
				}
			}
		}
	}
	if len(s.Tracks) == 0 {
		return nil, fmt.Errorf("sdp: no media sections")
	}
	base := s.Control
	if base == "" || base == "*" {
		base = baseURL
	}
	for _, t := range s.Tracks {
		t.URL = ResolveControl(base, t.Control)
		if t.ClockRate == 0 {
			t.ClockRate = 90000
		}
	}
	return s, nil
}

// ResolveControl turns an absolute or relative control attribute into a full
// track URL. "*" and empty values resolve to the base itself.
func ResolveControl(base, control string) string {
	if control == "" || control == "*" {
		return base
	}
	if strings.Contains(control, "://") {
		return control
	}
	if strings.HasPrefix(control, "/") {
		if i := strings.Index(base, "://"); i >= 0 {
			rest := base[i+3:]
			if j := strings.IndexByte(rest, '/'); j >= 0 {
				return base[:i+3+j] + control
			}
			return base + control
		}
	}
	return strings.TrimSuffix(base, "/") + "/" + strings.TrimPrefix(control, "/")
}

func parseMedia(line string) (*Track, error) {
	// m=<media> <port> <proto> <fmt> ...
	f := strings.Fields(strings.TrimPrefix(line, "m="))
	if len(f) < 4 {
		return nil, fmt.Errorf("sdp: malformed m= line %q", line)
	}
	pt, err := strconv.ParseUint(f[3], 10, 8)
	if err != nil {
		return nil, fmt.Errorf("sdp: bad payload type in %q: %w", line, err)
	}
	return &Track{Media: f[0], PayloadType: uint8(pt)}, nil
}

func parseRTPMap(line string, t *Track) error {
	// a=rtpmap:<pt> <encoding>/<clock>[/...]
	v := strings.TrimPrefix(line, "a=rtpmap:")
	f := strings.Fields(v)
	if len(f) != 2 {
		return fmt.Errorf("sdp: malformed rtpmap %q", line)
	}
	pt, err := strconv.ParseUint(f[0], 10, 8)
	if err != nil || uint8(pt) != t.PayloadType {
		return nil // rtpmap for another payload format
	}
	parts := strings.Split(f[1], "/")
	if len(parts) >= 2 {
		rate, err := strconv.Atoi(parts[1])
		if err != nil {
			return fmt.Errorf("sdp: bad clock rate in %q: %w", line, err)
		}
		t.ClockRate = rate
	}
	return nil
}
