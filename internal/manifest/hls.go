package manifest

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"

	"mediac/internal/core"
)

// HLSScope limits the media sequence window kept from a media playlist.
type HLSScope struct {
	StartSeq int // first media sequence number to keep
	EndSeq   int // last media sequence number to keep; -1 = until end
}

type hlsVariant struct {
	URI    string
	ID     string
	BPS    int64
	Codecs string
	ResW   int
	ResH   int
	Line   int
}

// ParseHLS parses a master playlist in r and then each referenced media
// playlist via open. open receives a root-relative URI and must return its
// contents (the fixture layer supplies an in-memory opener).
func ParseHLS(root, masterPath string, r io.Reader,
	open func(uri string) (io.ReadCloser, error), scope HLSScope) (*Manifest, error) {
	raw, err := io.ReadAll(io.LimitReader(r, MaxManifestBytes))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) >= MaxManifestBytes {
		return nil, core.NewFault(core.FaultBlocked, core.SevBlocking,
			"master playlist exceeds size limit", core.Source{})
	}
	variants, err := parseHLSMaster(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	out := &Manifest{Kind: KindHLS, Root: root, SourcePath: masterPath}
	for _, v := range variants {
		if _, err := hlsSafeRef(root, v.URI); err != nil {
			return nil, core.NewFault(core.FaultPathEscape,
				core.SevBlocking, "variant ref "+v.URI, core.Source{Line: v.Line})
		}
		rc, err := open(v.URI)
		if err != nil {
			return nil, core.NewFault(core.FaultMissingInit,
				core.SevBlocking, "cannot open media playlist: "+err.Error(),
				core.Source{RepID: v.ID, Line: v.Line})
		}
		rep, err := parseHLSMedia(root, v, rc, scope)
		rc.Close()
		if err != nil {
			return nil, err
		}
		out.Representations = append(out.Representations, *rep)
	}
	if len(out.Representations) == 0 && len(out.Faults) == 0 {
		return nil, core.NewFault(core.FaultBlocked, core.SevBlocking,
			"HLS master lists no variants", core.Source{})
	}
	return out, nil
}

func parseHLSMaster(r io.Reader) ([]hlsVariant, error) {
	sc := bufio.NewScanner(io.LimitReader(r, MaxManifestBytes))
	sc.Buffer(make([]byte, 0, MaxLineBytes), MaxLineBytes)
	var variants []hlsVariant
	var pending map[string]string
	pendingLine := 0
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#EXT-X-STREAM-INF") {
			pending = parseHLSTag(line)
			pendingLine = lineNo
			continue
		}
		if strings.HasPrefix(line, "#") {
			continue
		}
		if pending == nil {
			continue // media segment URI outside a media playlist
		}
		v := hlsVariant{URI: line, Line: pendingLine, BPS: atoi64(pending["BANDWIDTH"]),
			Codecs: pending["CODECS"]}
		if res := pending["RESOLUTION"]; res != "" {
			fmt.Sscanf(res, "%dx%d", &v.ResW, &v.ResH)
		}
		v.ID = orDefault(pending["VIDEO"], fmt.Sprintf("variant%d", len(variants)+1))
		variants = append(variants, v)
		pending = nil
	}
	return variants, sc.Err()
}

func parseHLSMedia(root string, v hlsVariant, r io.Reader,
	scope HLSScope) (*Representation, error) {
	sc := bufio.NewScanner(io.LimitReader(r, MaxManifestBytes))
	sc.Buffer(make([]byte, 0, MaxLineBytes), MaxLineBytes)
	rep := &Representation{
		ID: v.ID, Codec: v.Codecs, Width: v.ResW, Height: v.ResH,
		TargetBPS: v.BPS, Timescale: 1000,
	}
	mediaSeq := 0
	hasSeq := false
	cursorMs := int64(0)
	pendingDur := int64(-1)
	pendingLine := 0
	lineNo := 0
	lastStart := int64(math.MinInt64)
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		switch {
		case strings.HasPrefix(line, "#EXT-X-MEDIA-SEQUENCE"):
			mediaSeq = int(atoi64(tagValue(line)))
			hasSeq = true
		case strings.HasPrefix(line, "#EXT-X-MAP"):
			uri := attrAfter(line, "URI=")
			p, err := hlsSafeRef(root, uri)
			if err != nil {
				return nil, core.NewFault(core.FaultPathEscape, core.SevBlocking,
					"map ref "+uri, core.Source{RepID: rep.ID, Line: lineNo})
			}
			rep.InitURL, rep.InitPath = uri, p
		case strings.HasPrefix(line, "#EXTINF"):
			secs, err := strconv.ParseFloat(strings.TrimPrefix(strings.Split(tagValue(line), ",")[0], ""), 64)
			if err != nil {
				return nil, core.NewFault(core.FaultBlocked, core.SevBlocking,
					"bad EXTINF: "+err.Error(), core.Source{RepID: rep.ID, Line: lineNo})
			}
			pendingDur = int64(math.Round(secs * 1000))
			if pendingDur < 0 {
				return nil, core.NewFault(core.FaultTimestampBackward, core.SevDamaged,
					fmt.Sprintf("negative EXTINF %.3f rewinds the media clock", secs),
					core.Source{RepID: rep.ID, SegmentIndex: mediaSeq, Line: lineNo})
			}
			pendingLine = lineNo
		case line == "#EXT-X-DISCONTINUITY":
			rep.Discontinuities = append(rep.Discontinuities, Discontinuity{
				Index: mediaSeq, Line: lineNo, StartMs: cursorMs,
			})
			lastStart = math.MinInt64 // clock reset: suppress backward check
		case !strings.HasPrefix(line, "#") && line != "":
			if pendingDur < 0 {
				return nil, core.NewFault(core.FaultBlocked, core.SevBlocking,
					"segment URI without EXTINF",
					core.Source{RepID: rep.ID, Line: lineNo})
			}
			if hasSeq && scope.EndSeq >= 0 && mediaSeq > scope.EndSeq {
				return rep, nil
			}
			if !hasSeq || mediaSeq >= scope.StartSeq && (scope.EndSeq < 0 || mediaSeq <= scope.EndSeq) {
				if cursorMs < lastStart {
					return nil, core.NewFault(core.FaultTimestampBackward, core.SevDamaged,
						fmt.Sprintf("segment %d starts before predecessor", mediaSeq),
						core.Source{RepID: rep.ID, SegmentIndex: mediaSeq, Line: pendingLine})
				}
				p, err := hlsSafeRef(root, line)
				if err != nil {
					return nil, core.NewFault(core.FaultPathEscape, core.SevBlocking,
						"segment ref "+line,
						core.Source{RepID: rep.ID, SegmentIndex: mediaSeq, Line: lineNo})
				}
				rep.Segments = append(rep.Segments, Segment{
					Index: mediaSeq, URL: line, AbsPath: p,
					StartMs: cursorMs, DurationMs: pendingDur, Line: pendingLine,
				})
				lastStart = cursorMs
				cursorMs += pendingDur
			} else {
				cursorMs += pendingDur // skipped window still advances the clock
			}
			mediaSeq++
			pendingDur = -1
		}
	}
	if rep.InitPath == "" {
		return nil, core.NewFault(core.FaultMissingInit, core.SevBlocking,
			"HLS media playlist has no EXT-X-MAP initialization",
			core.Source{RepID: rep.ID})
	}
	return rep, sc.Err()
}

func hlsSafeRef(root, ref string) (string, error) {
	return core.SafeJoin(root, ref)
}

// parseHLSTag splits "#EXT-X-...:KEY=VALUE,KEY2=..." into a key map. Quoted
// values keep their contents without quotes.
func parseHLSTag(line string) map[string]string {
	out := map[string]string{}
	colon := strings.IndexByte(line, ':')
	if colon < 0 {
		return out
	}
	for _, part := range splitCSV(line[colon+1:]) {
		eq := strings.IndexByte(part, '=')
		if eq < 0 {
			continue
		}
		out[strings.TrimSpace(part[:eq])] = strings.Trim(part[eq+1:], "\"'")
	}
	return out
}

func splitCSV(s string) []string {
	var parts []string
	depth := 0
	start := 0
	for i, r := range s {
		switch r {
		case '"':
			depth ^= 1
		case ',':
			if depth == 0 {
				parts = append(parts, s[start:i])
				start = i + 1
			}
		}
	}
	return append(parts, s[start:])
}

func tagValue(line string) string {
	if i := strings.IndexByte(line, ':'); i >= 0 {
		return line[i+1:]
	}
	return ""
}

func atoi64(s string) int64 {
	n, _ := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	return n
}
