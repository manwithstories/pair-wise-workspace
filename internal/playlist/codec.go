package playlist

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// MasterPlaylist is the subset of a multivariant playlist used by m3u8ctl.
type MasterPlaylist struct {
	Version             int
	IndependentSegments bool
	Variants            []Variant
}

// Variant describes one rendition and its media playlist URI.
type Variant struct {
	Name             string
	URI              string
	Bandwidth        int64
	AverageBandwidth int64
	Resolution       string
	Codecs           string
}

// MediaPlaylist is a parsed media playlist.
type MediaPlaylist struct {
	Version               int
	TargetDuration        int
	MediaSequence         int64
	DiscontinuitySequence int64
	PlaylistType          string
	Segments              []MediaSegment
	EndList               bool
}

// MediaSegment is one media segment entry.
type MediaSegment struct {
	URI           string
	Duration      float64
	Discontinuity bool
}

// ParseMasterFile reads a master playlist from disk.
func ParseMasterFile(path string) (*MasterPlaylist, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return ParseMaster(f)
}

// ParseMaster decodes a master playlist.
func ParseMaster(r io.Reader) (*MasterPlaylist, error) {
	lines, err := scanLines(r)
	if err != nil {
		return nil, err
	}
	p := &MasterPlaylist{Version: 3}
	var pending *Attributes
	seenHeader := false
	for n, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		switch {
		case line == "#EXTM3U":
			seenHeader = true
		case strings.HasPrefix(line, "#EXT-X-VERSION:"):
			p.Version = parseInt(strings.TrimPrefix(line, "#EXT-X-VERSION:"), n+1)
		case line == "#EXT-X-INDEPENDENT-SEGMENTS":
			p.IndependentSegments = true
		case strings.HasPrefix(line, "#EXT-X-STREAM-INF:"):
			attrs, err := ParseAttributes(strings.TrimPrefix(line, "#EXT-X-STREAM-INF:"))
			if err != nil {
				return nil, fmt.Errorf("line %d: %w", n+1, err)
			}
			pending = attrs
		case strings.HasPrefix(line, "#"):
			continue
		default:
			if pending == nil {
				return nil, fmt.Errorf("line %d: URI without EXT-X-STREAM-INF", n+1)
			}
			v := Variant{URI: line}
			v.Bandwidth = attrInt(pending, "BANDWIDTH")
			v.AverageBandwidth = attrInt(pending, "AVERAGE-BANDWIDTH")
			v.Resolution, _ = pending.Get("RESOLUTION")
			v.Codecs, _ = pending.Get("CODECS")
			v.Name, _ = pending.Get("NAME")
			if v.Name == "" {
				v.Name = strings.TrimSuffix(strings.TrimSuffix(line, ".m3u8"), "/index")
			}
			p.Variants = append(p.Variants, v)
			pending = nil
		}
	}
	if !seenHeader {
		return nil, fmt.Errorf("missing #EXTM3U")
	}
	if pending != nil {
		return nil, fmt.Errorf("EXT-X-STREAM-INF without URI")
	}
	if len(p.Variants) == 0 {
		return nil, fmt.Errorf("master playlist has no variants")
	}
	return p, nil
}

// ParseMediaFile reads a media playlist from disk.
func ParseMediaFile(path string) (*MediaPlaylist, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return ParseMedia(f)
}

// ParseMedia decodes a media playlist.
func ParseMedia(r io.Reader) (*MediaPlaylist, error) {
	lines, err := scanLines(r)
	if err != nil {
		return nil, err
	}
	p := &MediaPlaylist{Version: 3}
	seenHeader := false
	var duration float64
	haveDuration := false
	discontinuity := false
	for n, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		switch {
		case line == "#EXTM3U":
			seenHeader = true
		case strings.HasPrefix(line, "#EXT-X-VERSION:"):
			p.Version = parseInt(strings.TrimPrefix(line, "#EXT-X-VERSION:"), n+1)
		case strings.HasPrefix(line, "#EXT-X-TARGETDURATION:"):
			p.TargetDuration = parseInt(strings.TrimPrefix(line, "#EXT-X-TARGETDURATION:"), n+1)
		case strings.HasPrefix(line, "#EXT-X-MEDIA-SEQUENCE:"):
			p.MediaSequence = parseInt64(strings.TrimPrefix(line, "#EXT-X-MEDIA-SEQUENCE:"), n+1)
		case strings.HasPrefix(line, "#EXT-X-DISCONTINUITY-SEQUENCE:"):
			p.DiscontinuitySequence = parseInt64(strings.TrimPrefix(line, "#EXT-X-DISCONTINUITY-SEQUENCE:"), n+1)
		case strings.HasPrefix(line, "#EXT-X-PLAYLIST-TYPE:"):
			p.PlaylistType = strings.TrimSpace(strings.TrimPrefix(line, "#EXT-X-PLAYLIST-TYPE:"))
		case line == "#EXT-X-DISCONTINUITY":
			discontinuity = true
		case strings.HasPrefix(line, "#EXTINF:"):
			value := strings.TrimPrefix(line, "#EXTINF:")
			if comma := strings.IndexByte(value, ','); comma >= 0 {
				value = value[:comma]
			}
			duration, err = strconv.ParseFloat(strings.TrimSpace(value), 64)
			if err != nil || duration < 0 {
				return nil, fmt.Errorf("line %d: invalid EXTINF", n+1)
			}
			haveDuration = true
		case line == "#EXT-X-ENDLIST":
			p.EndList = true
		case strings.HasPrefix(line, "#"):
			continue
		default:
			if !haveDuration {
				return nil, fmt.Errorf("line %d: segment URI without EXTINF", n+1)
			}
			p.Segments = append(p.Segments, MediaSegment{URI: line, Duration: duration, Discontinuity: discontinuity})
			haveDuration = false
			discontinuity = false
		}
	}
	if !seenHeader {
		return nil, fmt.Errorf("missing #EXTM3U")
	}
	if haveDuration {
		return nil, fmt.Errorf("EXTINF without segment URI")
	}
	return p, nil
}

// Write serializes a master playlist in a stable tag and attribute order.
func (p *MasterPlaylist) Write(w io.Writer) error {
	bw := bufio.NewWriter(w)
	version := p.Version
	if version == 0 {
		version = 3
	}
	fmt.Fprintf(bw, "#EXTM3U\n#EXT-X-VERSION:%d\n", version)
	if p.IndependentSegments {
		fmt.Fprintln(bw, "#EXT-X-INDEPENDENT-SEGMENTS")
	}
	for _, v := range p.Variants {
		attrs := NewAttributes()
		attrs.Add("BANDWIDTH", strconv.FormatInt(v.Bandwidth, 10), false)
		if v.AverageBandwidth > 0 {
			attrs.Add("AVERAGE-BANDWIDTH", strconv.FormatInt(v.AverageBandwidth, 10), false)
		}
		if v.Resolution != "" {
			attrs.Add("RESOLUTION", v.Resolution, false)
		}
		if v.Codecs != "" {
			attrs.Add("CODECS", v.Codecs, true)
		}
		attrs.Add("NAME", v.Name, true)
		fmt.Fprintf(bw, "#EXT-X-STREAM-INF:%s\n%s\n", attrs, v.URI)
	}
	return bw.Flush()
}

// Write serializes a media playlist with three-decimal EXTINF values.
func (p *MediaPlaylist) Write(w io.Writer) error {
	bw := bufio.NewWriter(w)
	version := p.Version
	if version == 0 {
		version = 3
	}
	fmt.Fprintf(bw, "#EXTM3U\n#EXT-X-VERSION:%d\n", version)
	fmt.Fprintf(bw, "#EXT-X-TARGETDURATION:%d\n", p.TargetDuration)
	fmt.Fprintf(bw, "#EXT-X-MEDIA-SEQUENCE:%d\n", p.MediaSequence)
	fmt.Fprintf(bw, "#EXT-X-DISCONTINUITY-SEQUENCE:%d\n", p.DiscontinuitySequence)
	if p.PlaylistType != "" {
		fmt.Fprintf(bw, "#EXT-X-PLAYLIST-TYPE:%s\n", p.PlaylistType)
	}
	for _, s := range p.Segments {
		if s.Discontinuity {
			fmt.Fprintln(bw, "#EXT-X-DISCONTINUITY")
		}
		fmt.Fprintf(bw, "#EXTINF:%.3f,\n%s\n", s.Duration, s.URI)
	}
	if p.EndList {
		fmt.Fprintln(bw, "#EXT-X-ENDLIST")
	}
	return bw.Flush()
}

func scanLines(r io.Reader) ([]string, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	var lines []string
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	return lines, scanner.Err()
}

func parseInt(s string, line int) int {
	v, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0
	}
	return v
}

func parseInt64(s string, line int) int64 {
	v, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0
	}
	return v
}

func attrInt(attrs *Attributes, key string) int64 {
	value, _ := attrs.Get(key)
	v, _ := strconv.ParseInt(value, 10, 64)
	return v
}
