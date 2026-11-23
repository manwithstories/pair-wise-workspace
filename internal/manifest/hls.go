package manifest

import (
	"bufio"
	"fmt"
	"strconv"
	"strings"

	"mediascan/internal/clock"
)

// hlsState carries the tags that apply to the following segments.
type hlsState struct {
	seq           int64
	durationNs    int64 // pending EXTINF
	byteRange     *ByteRange
	discontinuity bool
	initURI       string
}

type hlsVariant struct {
	uri       string
	bandwidth int64
	codecs    string
}

// hlsPlaylist is the intermediate representation of one HLS document.
type hlsPlaylist struct {
	variants    []hlsVariant
	segments    []Segment
	targetDurNs int64
	mediaSeq    int64
	hasMediaSeq bool
	endList     bool
	warnings    []string
}

// parseHLS parses either a master or a media playlist.
func parseHLS(data []byte, opts Options) (*hlsPlaylist, error) {
	opts = opts.normalize()
	if len(data) > opts.MaxBytes {
		return nil, fmt.Errorf("%w: %d bytes", ErrTooLarge, len(data))
	}
	p := &hlsPlaylist{}
	sc := bufio.NewScanner(strings.NewReader(string(data)))
	sc.Buffer(make([]byte, 0, 64*1024), opts.MaxBytes)
	line := 0
	var st hlsState
	for sc.Scan() {
		line++
		raw := strings.TrimSpace(sc.Text())
		if raw == "" {
			continue
		}
		if strings.HasPrefix(raw, "#") {
			if err := p.tag(raw, &st); err != nil {
				return nil, fmt.Errorf("line %d: %w", line, err)
			}
			continue
		}
		if n := len(p.variants); n > 0 && p.variants[n-1].uri == "" {
			if err := ValidateURI(raw); err != nil {
				return nil, fmt.Errorf("line %d: %w", line, err)
			}
			p.variants[n-1].uri = raw
			continue
		}
		if len(p.variants) > 0 {
			return nil, fmt.Errorf("line %d: %w: URI after master variant list", line, ErrInvalid)
		}
		seg, err := p.segment(raw, &st)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		if len(p.segments) >= opts.MaxSegments {
			return nil, fmt.Errorf("%w: more than %d segments", ErrTooLarge, opts.MaxSegments)
		}
		p.segments = append(p.segments, seg)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if len(p.variants) == 0 && len(p.segments) == 0 {
		return nil, fmt.Errorf("%w: playlist has no segments", ErrInvalid)
	}
	for _, v := range p.variants {
		if v.uri == "" {
			return nil, fmt.Errorf("%w: STREAM-INF without URI", ErrInvalid)
		}
	}
	return p, nil
}

func splitTag(raw string) (name, value string) {
	body := strings.TrimPrefix(raw, "#")
	if i := strings.IndexByte(body, ':'); i >= 0 {
		return body[:i], body[i+1:]
	}
	return body, ""
}

func (p *hlsPlaylist) tag(raw string, st *hlsState) error {
	name, value := splitTag(raw)
	switch name {
	case "EXTM3U", "EXT-X-VERSION", "EXT-X-PLAYLIST-TYPE", "EXT-X-I-FRAMES-ONLY",
		"EXT-X-INDEPENDENT-SEGMENTS", "EXT-X-SESSION-DATA", "EXT-X-SESSION-KEY",
		"EXT-X-START", "EXT-X-SERVER-CONTROL", "EXT-X-PART-INF", "EXT-X-KEY",
		"EXT-X-PROGRAM-DATE-TIME", "EXT-X-GAP":
		return nil
	case "EXT-X-MEDIA-SEQUENCE":
		n, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if err != nil || n < 0 {
			return fmt.Errorf("%w: bad media sequence %q", ErrInvalid, value)
		}
		st.seq = n
		p.mediaSeq = n
		p.hasMediaSeq = true
	case "EXT-X-DISCONTINUITY":
		st.discontinuity = true
	case "EXT-X-DISCONTINUITY-SEQUENCE":
		if _, err := strconv.Atoi(strings.TrimSpace(value)); err != nil {
			return fmt.Errorf("%w: bad discontinuity sequence", ErrInvalid)
		}
	case "EXT-X-TARGETDURATION":
		secs, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err != nil || secs <= 0 {
			return fmt.Errorf("%w: bad target duration", ErrInvalid)
		}
		p.targetDurNs = int64(secs * 1e9)
	case "EXT-X-ENDLIST":
		p.endList = true
	case "EXT-X-MAP":
		uri, err := attrString(value, "URI")
		if err != nil {
			return err
		}
		if err := ValidateURI(uri); err != nil {
			return err
		}
		st.initURI = uri
	case "EXTINF":
		num := value
		if i := strings.IndexByte(num, ','); i >= 0 {
			num = num[:i]
		}
		secs, err := strconv.ParseFloat(strings.TrimSpace(num), 64)
		if err != nil || secs <= 0 {
			return fmt.Errorf("%w: bad EXTINF %q", ErrInvalid, value)
		}
		st.durationNs = int64(secs * 1e9)
	case "EXT-X-BYTERANGE":
		br, err := parseByteRange(value)
		if err != nil {
			return err
		}
		st.byteRange = br
	case "EXT-X-STREAM-INF":
		bw, err := attrInt(value, "BANDWIDTH")
		if err != nil {
			return err
		}
		codecs, _ := attrString(value, "CODECS")
		p.variants = append(p.variants, hlsVariant{bandwidth: bw, codecs: codecs})
	default:
		if strings.HasPrefix(name, "EXT-X-") {
			p.warnings = append(p.warnings, "ignored tag "+name)
		}
	}
	return nil
}

func (p *hlsPlaylist) segment(raw string, st *hlsState) (Segment, error) {
	if st.durationNs <= 0 {
		return Segment{}, fmt.Errorf("%w: segment %q has no EXTINF", ErrInvalid, raw)
	}
	if err := ValidateURI(raw); err != nil {
		return Segment{}, err
	}
	seg := Segment{
		URI:           raw,
		Sequence:      st.seq,
		DurationNs:    st.durationNs,
		Discontinuity: st.discontinuity,
		ByteRange:     st.byteRange,
		InitURI:       st.initURI,
	}
	st.seq++
	st.durationNs = 0
	st.byteRange = nil
	st.discontinuity = false
	return seg, nil
}

// toPlans builds the media plan and assigns StartNs from EXTINF durations.
func (p *hlsPlaylist) toPlans() []*Plan {
	if len(p.variants) > 0 {
		return nil
	}
	segs := make([]Segment, len(p.segments))
	copy(segs, p.segments)
	var t int64
	disc := 0
	for i := range segs {
		if segs[i].Discontinuity {
			disc++
		}
		segs[i].StartNs = t
		t += segs[i].DurationNs
	}
	return []*Plan{{
		Format:          FormatHLS,
		Representation:  "media",
		MimeType:        "video/mp4",
		Timescale:       clock.DefaultTimescale,
		Segments:        segs,
		DurationNs:      t,
		Discontinuities: disc,
		Warnings:        p.warnings,
	}}
}

func parseByteRange(v string) (*ByteRange, error) {
	parts := strings.SplitN(strings.TrimSpace(v), "@", 2)
	length, err := strconv.ParseInt(strings.TrimSpace(parts[0]), 10, 64)
	if err != nil || length <= 0 {
		return nil, fmt.Errorf("%w: bad byte range %q", ErrInvalid, v)
	}
	br := &ByteRange{Length: length}
	if len(parts) == 2 {
		off, err := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
		if err != nil || off < 0 {
			return nil, fmt.Errorf("%w: bad byte range offset %q", ErrInvalid, v)
		}
		br.Offset = off
	}
	return br, nil
}

// attrString extracts a quoted or bare attribute from an attribute list.
func attrString(list, key string) (string, error) {
	v, ok := findAttr(list, key)
	if !ok {
		return "", fmt.Errorf("%w: missing attribute %s", ErrInvalid, key)
	}
	return strings.Trim(strings.TrimSpace(v), `"`), nil
}

func attrInt(list, key string) (int64, error) {
	v, ok := findAttr(list, key)
	if !ok {
		return 0, fmt.Errorf("%w: missing attribute %s", ErrInvalid, key)
	}
	n, err := strconv.ParseInt(strings.Trim(strings.TrimSpace(v), `"`), 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%w: bad numeric attribute %s=%q", ErrInvalid, key, v)
	}
	return n, nil
}

// findAttr scans a comma-separated attribute list while respecting quotes.
func findAttr(list, key string) (string, bool) {
	inQuote := false
	start := 0
	for i := 0; i <= len(list); i++ {
		atEnd := i == len(list)
		if !atEnd && list[i] == '"' {
			inQuote = !inQuote
			continue
		}
		if atEnd || (list[i] == ',' && !inQuote) {
			part := strings.TrimSpace(list[start:i])
			start = i + 1
			if eq := strings.IndexByte(part, '='); eq > 0 {
				if strings.EqualFold(strings.TrimSpace(part[:eq]), key) {
					return part[eq+1:], true
				}
			}
		}
	}
	return "", false
}
