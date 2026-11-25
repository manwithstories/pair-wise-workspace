package manifest

import (
	"bufio"
	"io"
	"os"
	"strconv"
	"strings"

	"mediad/internal/media"
)

// HLSVariant is one master-playlist entry.
type HLSVariant struct {
	URI       string
	Codecs    string
	Bandwidth int
	Width     int
	Height    int
	Language  string
	Pos       media.SourcePos
}

// HLSMedia is a parsed media playlist.
type HLSMedia struct {
	File             string
	TargetDur        media.Rational
	MediaSeq         int
	DiscontinuitySeq int
	Segments         []Segment
	EndList          bool
	cursorTicks      int64
}

// ReadHLS detects whether file is a master or media playlist and parses it.
// A master playlist is followed one level deep into its variants; each variant
// becomes a Rendition whose segments are tagged with that rendition.
func ReadHLS(file string, opts Options) (*Manifest, error) {
	data, err := readLimited(file, opts.limits())
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(strings.TrimSpace(string(data)), "#EXTM3U") {
		return nil, media.New(media.CodeManifest, "hls", media.SourcePos{File: file, Line: 1, Col: 1},
			"missing #EXTM3U header")
	}
	lines := splitLines(data)
	isMaster := false
	for _, ln := range lines {
		if strings.HasPrefix(ln.text, "#EXT-X-STREAM-INF") {
			isMaster = true
			break
		}
	}
	if isMaster {
		return readHLSMaster(file, lines, opts)
	}
	mp, err := parseHLSMedia(file, lines, opts)
	if err != nil {
		return nil, err
	}
	m := &Manifest{Format: "hls", File: file, Timescale: media.TickRate}
	m.Segments = mp.Segments
	if err := ValidateSegments(m.Segments); err != nil {
		return nil, err
	}
	return m, nil
}

func readHLSMaster(file string, lines []lineRec, opts Options) (*Manifest, error) {
	var variants []HLSVariant
	for i := 0; i < len(lines); i++ {
		ln := lines[i]
		if !strings.HasPrefix(ln.text, "#EXT-X-STREAM-INF:") {
			continue
		}
		attrs := parseAttributes(strings.TrimPrefix(ln.text, "#EXT-X-STREAM-INF:"))
		// The URI is the next non-comment, non-blank line.
		uri := ""
		uriLine := ln.num
		for j := i + 1; j < len(lines); j++ {
			if lines[j].text == "" || strings.HasPrefix(lines[j].text, "#") {
				continue
			}
			uri = lines[j].text
			uriLine = lines[j].num
			i = j
			break
		}
		if uri == "" {
			return nil, media.New(media.CodeMissingEntry, "hls-master", media.SourcePos{File: file, Line: ln.num, Col: 1},
				"variant %q has no URI line", ln.text)
		}
		if _, err := parseDurationAttr(attrs["FRAME-RATE"], media.SourcePos{File: file, Line: ln.num, Col: 1}); err != nil {
			return nil, err
		}
		v := HLSVariant{
			URI:       uri,
			Pos:       media.SourcePos{File: file, Line: uriLine, Col: 1},
			Codecs:    attrs["CODECS"],
			Bandwidth: atoiDefault(attrs["BANDWIDTH"]),
			Language:  attrs["LANGUAGE"],
		}
		if res, ok := attrs["RESOLUTION"]; ok {
			if w, h, ok := parseResolution(res); ok {
				v.Width, v.Height = w, h
			}
		}
		variants = append(variants, v)
	}
	if len(variants) == 0 {
		return nil, media.New(media.CodeMissingEntry, "hls-master", media.SourcePos{File: file, Line: 1, Col: 1},
			"master playlist declares no variants")
	}
	lim := opts.limits()
	if err := lim.CheckCount("segments", 0); err != nil {
		return nil, err
	}

	out := &Manifest{Format: "hls", File: file, Timescale: media.TickRate}
	seqBase := 0
	for vi, v := range variants {
		abs, _, err := ResolveURI(opts.Root, file, v.URI, v.Pos)
		if err != nil {
			return nil, err
		}
		vdata, err := readLimited(abs, lim)
		if err != nil {
			return nil, err
		}
		vlines := splitLines(vdata)
		mp, err := parseHLSMedia(abs, vlines, opts)
		if err != nil {
			return nil, err
		}
		rend := &Rendition{
			Camera:    cameraForVariant(v, vi),
			Language:  defaultString(v.Language, "und"),
			Codec:     defaultString(v.Codecs, "avc1"),
			Role:      defaultString(roleForVariant(v), "main"),
			Bandwidth: v.Bandwidth,
			Width:     v.Width,
			Height:    v.Height,
			URI:       v.URI,
			Type:      variantType(v.URI),
		}
		out.Renditions = append(out.Renditions, rend)
		for si := range mp.Segments {
			s := mp.Segments[si]
			s.Seq = seqBase + si
			s.Rendition = rend
			out.Segments = append(out.Segments, s)
			if err := checkSegLimit(lim, len(out.Segments), s.Pos); err != nil {
				return nil, err
			}
		}
		seqBase += len(mp.Segments)
	}
	if err := ValidateSegments(out.Segments); err != nil {
		return nil, err
	}
	return out, nil
}

func parseHLSMedia(file string, lines []lineRec, opts Options) (*HLSMedia, error) {
	mp := &HLSMedia{File: file}
	var pending *pendingSegment
	lim := opts.limits()
	discontinuity := 0
	for _, ln := range lines {
		text := ln.text
		if text == "" {
			continue
		}
		switch {
		case strings.HasPrefix(text, "#EXTINF:"):
			d, err := parseEXTINF(text, media.SourcePos{File: file, Line: ln.num, Col: 1})
			if err != nil {
				return nil, err
			}
			pending = &pendingSegment{dur: d, line: ln.num}
		case strings.HasPrefix(text, "#EXT-X-MEDIA-SEQUENCE:"):
			v := strings.TrimPrefix(text, "#EXT-X-MEDIA-SEQUENCE:")
			n, err := strconv.Atoi(strings.TrimSpace(v))
			if err != nil || n < 0 {
				return nil, media.New(media.CodeManifest, "hls", media.SourcePos{File: file, Line: ln.num, Col: 1},
					"invalid media sequence %q", v)
			}
			mp.MediaSeq = n
		case strings.HasPrefix(text, "#EXT-X-TARGETDURATION:"):
			v := strings.TrimPrefix(text, "#EXT-X-TARGETDURATION:")
			secs, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
			if err != nil || secs <= 0 {
				return nil, media.New(media.CodeInvalidDuration, "hls", media.SourcePos{File: file, Line: ln.num, Col: 1},
					"invalid target duration %q", v)
			}
			mp.TargetDur = media.MustRational(int64(secs*1000), 1000)
		case strings.HasPrefix(text, "#EXT-X-DISCONTINUITY") && !strings.HasPrefix(text, "#EXT-X-DISCONTINUITY-SEQUENCE"):
			discontinuity++
		case strings.HasPrefix(text, "#EXT-X-DISCONTINUITY-SEQUENCE:"):
			v := strings.TrimPrefix(text, "#EXT-X-DISCONTINUITY-SEQUENCE:")
			if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n >= 0 {
				discontinuity = n
			}
		case strings.HasPrefix(text, "#EXT-X-ENDLIST"):
			mp.EndList = true
		case strings.HasPrefix(text, "#"):
			// other tags ignored
		default:
			if pending == nil {
				return nil, media.New(media.CodeMissingEntry, "hls", media.SourcePos{File: file, Line: ln.num, Col: 1},
					"segment URI without preceding #EXTINF")
			}
			_, rel, err := ResolveURI(opts.Root, file, text, media.SourcePos{File: file, Line: ln.num, Col: 1})
			if err != nil {
				return nil, err
			}
			startTicks, err := pending.dur.RescaleTicks(media.TickRate)
			if err != nil {
				return nil, media.Wrap(media.CodeInvalidDuration, "hls", media.SourcePos{File: file, Line: ln.num, Col: 1}, err, "")
			}
			mp.Segments = append(mp.Segments, Segment{
				Seq:           mp.MediaSeq + len(mp.Segments),
				URI:           text,
				RelPath:       rel,
				Duration:      pending.dur,
				StartTicks:    mp.cursorTicks,
				Pos:           media.SourcePos{File: file, Line: ln.num, Col: 1},
				Discontinuity: discontinuity > 0,
			})
			mp.cursorTicks += startTicks
			pending = nil
			if err := checkSegLimit(lim, len(mp.Segments), media.SourcePos{File: file, Line: ln.num, Col: 1}); err != nil {
				return nil, err
			}
		}
	}
	if len(mp.Segments) == 0 {
		return nil, media.New(media.CodeMissingEntry, "hls", media.SourcePos{File: file, Line: 1, Col: 1},
			"media playlist declares no segments")
	}
	return mp, nil
}

type pendingSegment struct {
	dur  media.Rational
	line int
}

type lineRec struct {
	num  int
	text string
}

func splitLines(data []byte) []lineRec {
	sc := bufio.NewScanner(strings.NewReader(string(data)))
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	out := make([]lineRec, 0, 256)
	n := 0
	for sc.Scan() {
		n++
		out = append(out, lineRec{num: n, text: strings.TrimSpace(sc.Text())})
	}
	return out
}

func parseEXTINF(line string, pos media.SourcePos) (media.Rational, error) {
	v := strings.TrimPrefix(line, "#EXTINF:")
	if i := strings.IndexByte(v, ','); i >= 0 {
		v = v[:i]
	}
	v = strings.TrimSpace(v)
	if v == "" {
		return media.Rational{}, media.New(media.CodeInvalidDuration, "hls", pos, "empty EXTINF duration")
	}
	secs, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return media.Rational{}, media.New(media.CodeInvalidDuration, "hls", pos, "non-numeric duration %q", v)
	}
	if secs <= 0 || secs > 86400 {
		return media.Rational{}, media.New(media.CodeInvalidDuration, "hls", pos, "duration %q out of range", v)
	}
	// Preserve millisecond precision: EXTINF is decimal seconds.
	return media.MustRational(int64(secs*1000+0.5), 1000), nil
}

func parseDurationAttr(v string, pos media.SourcePos) (media.Rational, error) {
	if v == "" {
		return media.Rational{}, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f <= 0 {
		return media.Rational{}, media.New(media.CodeInvalidDuration, "hls-attr", pos, "invalid FRAME-RATE %q", v)
	}
	return media.MustRational(int64(f*1000+0.5), 1000), nil
}

func parseAttributes(s string) map[string]string {
	out := make(map[string]string)
	var key strings.Builder
	var val strings.Builder
	inQuotes := false
	flush := func() {
		if key.Len() > 0 {
			out[strings.ToUpper(strings.TrimSpace(key.String()))] = strings.Trim(strings.TrimSpace(val.String()), `"`)
		}
		key.Reset()
		val.Reset()
	}
	seenEq := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"':
			inQuotes = !inQuotes
			val.WriteByte(c)
		case c == '=' && !inQuotes && !seenEq:
			seenEq = true
		case c == ',' && !inQuotes:
			flush()
			seenEq = false
		default:
			if seenEq {
				val.WriteByte(c)
			} else {
				key.WriteByte(c)
			}
		}
	}
	flush()
	return out
}

func parseResolution(s string) (w, h int, ok bool) {
	parts := strings.SplitN(strings.ToLower(strings.TrimSpace(s)), "x", 2)
	if len(parts) != 2 {
		return 0, 0, false
	}
	w, err1 := strconv.Atoi(parts[0])
	h, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return w, h, true
}

func defaultString(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

func cameraForVariant(v HLSVariant, idx int) string {
	// Convention: the variant URI stem carries the camera id, e.g. cam2/main.m3u8.
	base := strings.TrimSuffix(strings.TrimSuffix(v.URI, "/"), ".m3u8")
	base = strings.TrimSuffix(base, "/main")
	parts := strings.Split(base, "/")
	for i := len(parts) - 1; i >= 0; i-- {
		p := strings.TrimSpace(parts[i])
		if p != "" {
			return p
		}
	}
	return "cam" + strconv.Itoa(idx+1)
}

func roleForVariant(v HLSVariant) string {
	low := strings.ToLower(v.URI)
	switch {
	case strings.Contains(low, "side"):
		return "side"
	case strings.Contains(low, "roam"), strings.Contains(low, "handheld"):
		return "roaming"
	default:
		return "main"
	}
}

func variantType(uri string) string {
	if strings.Contains(strings.ToLower(uri), "audio") {
		return "audio"
	}
	return "video"
}

func atoiDefault(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0
	}
	return n
}

func readLimited(file string, lim media.Limits) ([]byte, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, media.Wrap(media.CodeIO, "open", media.SourcePos{File: file}, err, "")
	}
	defer f.Close()
	max := lim.MaxManifestBytes
	if max <= 0 {
		max = media.DefaultLimits().MaxManifestBytes
	}
	data, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, media.Wrap(media.CodeIO, "read", media.SourcePos{File: file}, err, "")
	}
	if int64(len(data)) > max {
		return nil, media.New(media.CodeLimit, "read", media.SourcePos{File: file}, "manifest exceeds %d bytes", max)
	}
	return data, nil
}
