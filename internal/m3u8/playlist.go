package m3u8

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Segment is one media segment entry of a media playlist.
type Segment struct {
	DurationMs int
	URI        string
	CRC        uint32
	HasCRC     bool
	Gap        bool
}

// Media is the in-memory model of a media playlist.
type Media struct {
	Version        int
	TargetDuration int // seconds
	MediaSequence  int64
	PlaylistType   string
	Segments       []Segment
	EndList        bool
}

// CheckURI rejects absolute paths and directory traversal in playlist URIs.
func CheckURI(uri string) error {
	if uri == "" || strings.HasPrefix(uri, "/") || strings.Contains(uri, "\\") ||
		uri == ".." || strings.HasPrefix(uri, "../") || strings.Contains(uri, "/../") {
		return fmt.Errorf("m3u8: unsafe URI %q", uri)
	}
	return nil
}

// Validate enforces the EXTINF / TARGETDURATION rules and URI safety.
func (m *Media) Validate() error {
	if m.TargetDuration <= 0 {
		return fmt.Errorf("m3u8: TARGETDURATION must be positive")
	}
	limit := m.TargetDuration * 1000
	for i, s := range m.Segments {
		if s.DurationMs < 0 {
			return fmt.Errorf("m3u8: segment %d has negative EXTINF", i)
		}
		if s.DurationMs > limit {
			return fmt.Errorf("m3u8: segment %d EXTINF %dms exceeds TARGETDURATION %ds",
				i, s.DurationMs, m.TargetDuration)
		}
		if err := CheckURI(s.URI); err != nil {
			return fmt.Errorf("m3u8: segment %d: %w", i, err)
		}
	}
	return nil
}

// Render serializes the media playlist in strict canonical form.
func (m *Media) Render() string {
	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	fmt.Fprintf(&b, "#EXT-X-VERSION:%d\n", m.Version)
	fmt.Fprintf(&b, "#EXT-X-TARGETDURATION:%d\n", m.TargetDuration)
	fmt.Fprintf(&b, "#EXT-X-MEDIA-SEQUENCE:%d\n", m.MediaSequence)
	if m.PlaylistType != "" {
		fmt.Fprintf(&b, "#EXT-X-PLAYLIST-TYPE:%s\n", m.PlaylistType)
	}
	for _, s := range m.Segments {
		fmt.Fprintf(&b, "#EXTINF:%.3f,\n", float64(s.DurationMs)/1000)
		if s.HasCRC {
			fmt.Fprintf(&b, "#X-CRC32:%08x\n", s.CRC)
		}
		if s.Gap {
			b.WriteString("#EXT-X-GAP\n")
		}
		b.WriteString(s.URI + "\n")
	}
	if m.EndList {
		b.WriteString("#EXT-X-ENDLIST\n")
	}
	return b.String()
}

// ParseMedia parses a media playlist document.
func ParseMedia(doc string) (*Media, error) {
	lines := strings.Split(doc, "\n")
	if len(lines) == 0 || strings.TrimRight(lines[0], "\r") != "#EXTM3U" {
		return nil, fmt.Errorf("m3u8: missing #EXTM3U header")
	}
	m := &Media{}
	var pending *Segment
	ensure := func() *Segment {
		if pending == nil {
			pending = &Segment{}
		}
		return pending
	}
	for i, raw := range lines[1:] {
		l := ParseLine(raw)
		switch l.Kind {
		case KindTag:
			switch l.Tag {
			case "EXT-X-VERSION":
				m.Version, _ = strconv.Atoi(l.Value)
			case "EXT-X-TARGETDURATION":
				m.TargetDuration, _ = strconv.Atoi(l.Value)
			case "EXT-X-MEDIA-SEQUENCE":
				m.MediaSequence, _ = strconv.ParseInt(l.Value, 10, 64)
			case "EXT-X-PLAYLIST-TYPE":
				m.PlaylistType = l.Value
			case "EXTINF":
				f, err := strconv.ParseFloat(strings.TrimSuffix(l.Value, ","), 64)
				if err != nil {
					return nil, fmt.Errorf("m3u8: line %d: bad EXTINF %q", i+2, l.Value)
				}
				ensure().DurationMs = int(math.Round(f * 1000))
			case "EXT-X-GAP":
				ensure().Gap = true
			case "EXT-X-ENDLIST":
				m.EndList = true
			}
		case KindComment:
			if v, ok := strings.CutPrefix(l.Text, "#X-CRC32:"); ok {
				crc, err := strconv.ParseUint(v, 16, 32)
				if err != nil {
					return nil, fmt.Errorf("m3u8: line %d: bad X-CRC32 %q", i+2, v)
				}
				seg := ensure()
				seg.CRC, seg.HasCRC = uint32(crc), true
			}
		case KindURI:
			seg := ensure()
			seg.URI = l.Text
			m.Segments = append(m.Segments, *seg)
			pending = nil
		}
	}
	return m, nil
}

// Variant is one rendition entry of a master playlist.
type Variant struct {
	Name      string
	Bandwidth int
	Width     int
	Height    int
	URI       string
}

// Master is the in-memory model of a master playlist.
type Master struct {
	Version  int
	Variants []Variant
}

// Render serializes the master playlist in strict canonical form.
func (m *Master) Render() string {
	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	fmt.Fprintf(&b, "#EXT-X-VERSION:%d\n", m.Version)
	for _, v := range m.Variants {
		attrs := FormatAttrs([]Attr{
			{Key: "BANDWIDTH", Value: strconv.Itoa(v.Bandwidth)},
			{Key: "RESOLUTION", Value: fmt.Sprintf("%dx%d", v.Width, v.Height)},
			{Key: "NAME", Value: Quote(v.Name)},
		})
		fmt.Fprintf(&b, "#EXT-X-STREAM-INF:%s\n%s\n", attrs, v.URI)
	}
	return b.String()
}

// ParseMaster parses a master playlist document.
func ParseMaster(doc string) (*Master, error) {
	lines := strings.Split(doc, "\n")
	if len(lines) == 0 || strings.TrimRight(lines[0], "\r") != "#EXTM3U" {
		return nil, fmt.Errorf("m3u8: missing #EXTM3U header")
	}
	m := &Master{}
	var pending *Variant
	for i, raw := range lines[1:] {
		l := ParseLine(raw)
		switch l.Kind {
		case KindTag:
			switch l.Tag {
			case "EXT-X-VERSION":
				m.Version, _ = strconv.Atoi(l.Value)
			case "EXT-X-STREAM-INF":
				attrs, err := ParseAttrs(l.Value)
				if err != nil {
					return nil, fmt.Errorf("m3u8: line %d: %w", i+2, err)
				}
				v := Variant{}
				for _, a := range attrs {
					switch a.Key {
					case "BANDWIDTH":
						v.Bandwidth, _ = strconv.Atoi(a.Value)
					case "RESOLUTION":
						fmt.Sscanf(a.Value, "%dx%d", &v.Width, &v.Height)
					case "NAME":
						v.Name = Unquote(a.Value)
					}
				}
				pending = &v
			}
		case KindURI:
			if pending != nil {
				pending.URI = l.Text
				m.Variants = append(m.Variants, *pending)
				pending = nil
			}
		}
	}
	if len(m.Variants) == 0 {
		return nil, fmt.Errorf("m3u8: master playlist has no variants")
	}
	return m, nil
}
