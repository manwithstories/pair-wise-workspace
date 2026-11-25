// Package manifest parses local HLS playlists and static DASH MPD files.
package manifest

import (
	"bufio"
	"bytes"
	"fmt"
	"strconv"
	"strings"

	"fieldpost/internal/core"
)

// Kind distinguishes master, media and DASH manifests.
type Kind string

const (
	KindHLSMaster Kind = "hls-master"
	KindHLSMedia  Kind = "hls-media"
	KindDASH      Kind = "dash"
)

// Segment is one addressable media shard.
type Segment struct {
	Number    int64
	URI       string
	Duration  core.Ticks
	Offset    int64 // line number for HLS, XML byte offset for DASH
	ByteRange *ByteRange
}

// ByteRange is an HLS-style or DASH-style byte interval.
type ByteRange struct {
	Length int64
	Offset int64 // -1 when offset is implicit
}

// Variant is an HLS master playlist rendition.
type Variant struct {
	Bandwidth  int
	Resolution string
	URI        string
	Offset     int64
}

// Manifest is the parser's common manifest description.
type Manifest struct {
	Kind           Kind
	Source         string
	TargetDuration core.Ticks
	MediaSequence  int64
	Segments       []Segment
	Variants       []Variant
	DASH           *DASHInfo
}

func lineOffset(data []byte, pos int) int64 {
	return int64(bytes.Count(data[:min(pos, len(data))], []byte{'\n'}) + 1)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ParseHLS parses either a master or media HLS playlist.
func ParseHLS(source string, data []byte) (*Manifest, error) {
	if !bytes.HasPrefix(bytes.TrimLeft(data, "\xef\xbb\xbf"), []byte("#EXTM3U")) {
		return nil, core.ManifestError(source, 1, "missing #EXTM3U header")
	}
	m := &Manifest{Kind: KindHLSMedia, Source: source, MediaSequence: 0}
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	lineNo := 0
	seq := int64(0)
	var pendingDur core.Ticks = -1
	var pendingOff int64
	var pendingExplicit *int64
	var pendingRange *ByteRange
	var streamAttrs string
	var streamOff int64
	haveExtInf := false
	targetSet := false

	for sc.Scan() {
		lineNo++
		raw := strings.TrimSpace(sc.Text())
		if raw == "" || strings.HasPrefix(raw, "#EXT-X-") == false && strings.HasPrefix(raw, "#") {
			continue
		}
		if strings.HasPrefix(raw, "#EXT-X-VERSION") {
			continue
		}
		if strings.HasPrefix(raw, "#EXT-X-TARGETDURATION:") {
			v := strings.TrimPrefix(raw, "#EXT-X-TARGETDURATION:")
			secs, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			if err != nil || secs < 0 {
				return nil, core.ManifestError(source, int64(lineNo), "invalid TARGETDURATION %q", v)
			}
			m.TargetDuration = core.Ticks(secs) * core.Ticks(core.Scale)
			targetSet = true
			continue
		}
		if strings.HasPrefix(raw, "#EXT-X-MEDIA-SEQUENCE:") {
			v := strings.TrimPrefix(raw, "#EXT-X-MEDIA-SEQUENCE:")
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil || n < 0 {
				return nil, core.ManifestError(source, int64(lineNo), "invalid MEDIA-SEQUENCE %q", v)
			}
			m.MediaSequence = n
			seq = n
			continue
		}
		if strings.HasPrefix(raw, "#EXT-X-SEGMENT-NUMBER:") {
			v := strings.TrimPrefix(raw, "#EXT-X-SEGMENT-NUMBER:")
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil || n < 0 {
				return nil, core.ManifestError(source, int64(lineNo), "invalid segment number %q", v)
			}
			pendingExplicit = &n
			continue
		}
		if strings.HasPrefix(raw, "#EXTINF:") {
			rest := strings.TrimPrefix(raw, "#EXTINF:")
			comma := strings.IndexByte(rest, ',')
			durText := rest
			if comma >= 0 {
				durText = rest[:comma]
			}
			d, err := core.ParseSeconds(strings.TrimSpace(durText))
			if err != nil {
				return nil, core.ManifestError(source, int64(lineNo), "invalid EXTINF duration %q: %v", durText, err)
			}
			if d < 0 {
				return nil, core.ManifestError(source, int64(lineNo), "negative EXTINF duration")
			}
			pendingDur = d
			pendingOff = int64(lineNo)
			haveExtInf = true
			continue
		}
		if strings.HasPrefix(raw, "#EXT-X-BYTERANGE:") {
			br, err := parseByteRange(strings.TrimPrefix(raw, "#EXT-X-BYTERANGE:"))
			if err != nil {
				return nil, core.ManifestError(source, int64(lineNo), "%v", err)
			}
			pendingRange = br
			continue
		}
		if strings.HasPrefix(raw, "#EXT-X-STREAM-INF:") {
			streamAttrs = strings.TrimPrefix(raw, "#EXT-X-STREAM-INF:")
			streamOff = int64(lineNo)
			m.Kind = KindHLSMaster
			continue
		}
		if strings.HasPrefix(raw, "#") {
			continue
		}
		// URI line.
		off := int64(lineNo)
		if err := SafeURI(source, raw, off); err != nil {
			return nil, err
		}
		if streamAttrs != "" {
			v := Variant{URI: raw, Offset: streamOff}
			v.Bandwidth = attrInt(streamAttrs, "BANDWIDTH")
			v.Resolution = attrString(streamAttrs, "RESOLUTION")
			if v.Bandwidth <= 0 {
				return nil, core.ManifestError(source, streamOff, "STREAM-INF missing positive BANDWIDTH")
			}
			m.Variants = append(m.Variants, v)
			streamAttrs = ""
			continue
		}
		if !haveExtInf {
			return nil, core.ManifestError(source, off, "media URI without preceding EXTINF: %q", raw)
		}
		num := seq
		if pendingExplicit != nil {
			num = *pendingExplicit
		}
		m.Segments = append(m.Segments, Segment{
			Number: num, URI: raw, Duration: pendingDur, Offset: pendingOff, ByteRange: pendingRange,
		})
		seq++
		pendingDur, pendingExplicit, pendingRange = -1, nil, nil
		haveExtInf = false
	}
	if err := sc.Err(); err != nil {
		return nil, core.Wrap(core.StageManifest, core.ExitUsage, false, err, "scan HLS")
	}
	if m.Kind == KindHLSMaster {
		if len(m.Variants) == 0 {
			return nil, core.ManifestError(source, int64(lineNo), "master playlist contains no variants")
		}
		return m, nil
	}
	if !targetSet {
		return nil, core.ManifestError(source, 1, "media playlist missing EXT-X-TARGETDURATION")
	}
	if len(m.Segments) == 0 {
		return nil, core.ManifestError(source, int64(lineNo), "media playlist contains no segments")
	}
	nums := make([]int64, len(m.Segments))
	offs := make([]int64, len(m.Segments))
	for i, s := range m.Segments {
		nums[i], offs[i] = s.Number, s.Offset
		if s.Duration <= 0 {
			return nil, core.ManifestError(source, s.Offset, "segment %d has missing or non-positive duration", s.Number)
		}
	}
	if err := rejectDuplicateNumbers(source, nums, offs); err != nil {
		return nil, err
	}
	return m, nil
}

func parseByteRange(v string) (*ByteRange, error) {
	parts := strings.Split(v, "@")
	n, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || n <= 0 {
		return nil, fmt.Errorf("invalid BYTERANGE length %q", v)
	}
	br := &ByteRange{Length: n, Offset: -1}
	if len(parts) == 2 {
		o, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil || o < 0 {
			return nil, fmt.Errorf("invalid BYTERANGE offset %q", v)
		}
		br.Offset = o
	}
	return br, nil
}

func attrString(attrs, key string) string {
	for _, part := range strings.Split(attrs, ",") {
		kv := strings.SplitN(part, "=", 2)
		if len(kv) == 2 && strings.TrimSpace(kv[0]) == key {
			return strings.Trim(kv[1], "\"")
		}
	}
	return ""
}

func attrInt(attrs, key string) int {
	v := attrString(attrs, key)
	n, _ := strconv.Atoi(v)
	return n
}
