package manifest

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/pairwise/mediad/internal/media"
)

// HLSSegment is one media segment line and its preceding tags.
type HLSSegment struct {
	Index               int     `json:"index"`
	URI                 string  `json:"uri"`
	Duration            float64 `json:"duration"`
	DiscontinuityBefore bool    `json:"discontinuity_before"`
}

// HLSPlaylist is the constrained media playlist model.
type HLSPlaylist struct {
	Version         int          `json:"version"`
	TargetDuration  int          `json:"target_duration"`
	MediaSequence   int          `json:"media_sequence"`
	Segments        []HLSSegment `json:"segments"`
	Discontinuities []int        `json:"discontinuities"`
	EndList         bool         `json:"end_list"`
}

// ParseHLS parses the deterministic fixture profile.
func ParseHLS(data []byte) (*HLSPlaylist, error) {
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	lines := strings.Split(text, "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "#EXTM3U" {
		return nil, media.NewError(media.CodeMalformed, "manifest.hls", "#EXTM3U", "missing playlist header")
	}
	p := &HLSPlaylist{Version: 3}
	var pendingDuration float64
	var haveDuration bool
	pendingDiscontinuity := false
	segmentIndex := 0
	for lineNo, raw := range lines[1:] {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		switch {
		case line == "#EXT-X-ENDLIST":
			p.EndList = true
		case line == "#EXT-X-DISCONTINUITY":
			pendingDiscontinuity = true
		case strings.HasPrefix(line, "#EXT-X-VERSION:"):
			v, err := strconv.Atoi(strings.TrimPrefix(line, "#EXT-X-VERSION:"))
			if err != nil || v <= 0 {
				return nil, media.NewError(media.CodeMissingField, "manifest.hls", "EXT-X-VERSION", "invalid version")
			}
			p.Version = v
		case strings.HasPrefix(line, "#EXT-X-TARGETDURATION:"):
			v, err := strconv.Atoi(strings.TrimPrefix(line, "#EXT-X-TARGETDURATION:"))
			if err != nil || v <= 0 {
				return nil, media.NewError(media.CodeMissingField, "manifest.hls", "EXT-X-TARGETDURATION", "must be positive")
			}
			p.TargetDuration = v
		case strings.HasPrefix(line, "#EXT-X-MEDIA-SEQUENCE:"):
			v, err := strconv.Atoi(strings.TrimPrefix(line, "#EXT-X-MEDIA-SEQUENCE:"))
			if err != nil || v < 0 {
				return nil, media.NewError(media.CodeMissingField, "manifest.hls", "EXT-X-MEDIA-SEQUENCE", "invalid sequence")
			}
			p.MediaSequence = v
		case strings.HasPrefix(line, "#EXTINF:"):
			value := strings.TrimPrefix(line, "#EXTINF:")
			if i := strings.IndexByte(value, ','); i >= 0 {
				value = value[:i]
			}
			d, err := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(value), ","), 64)
			if err != nil || d <= 0 {
				return nil, media.Wrap(err, media.CodeMissingField, "manifest.hls", "EXTINF", "invalid duration")
			}
			pendingDuration, haveDuration = d, true
		case strings.HasPrefix(line, "#"):
			// Unknown tags are ignored by the narrow parser profile.
		default:
			if !haveDuration {
				return nil, media.NewError(media.CodeMissingField, "manifest.hls", fmt.Sprintf("segment[%d].EXTINF", segmentIndex), "segment URI without EXTINF")
			}
			if strings.Contains(line, "..") || strings.HasPrefix(line, "/") {
				return nil, media.NewError(media.CodePathViolation, "manifest.hls", line, "unsafe segment URI")
			}
			seg := HLSSegment{
				Index:               p.MediaSequence + segmentIndex,
				URI:                 line,
				Duration:            pendingDuration,
				DiscontinuityBefore: pendingDiscontinuity,
			}
			if pendingDiscontinuity {
				p.Discontinuities = append(p.Discontinuities, seg.Index)
			}
			p.Segments = append(p.Segments, seg)
			segmentIndex++
			haveDuration = false
			pendingDiscontinuity = false
		}
		_ = lineNo
	}
	if p.TargetDuration == 0 {
		return nil, media.NewError(media.CodeMissingField, "manifest.hls", "EXT-X-TARGETDURATION", "required tag missing")
	}
	if len(p.Segments) == 0 {
		return nil, media.NewError(media.CodeMissingField, "manifest.hls", "segments", "playlist contains no segments")
	}
	return p, nil
}

// NormalizeHLS assigns contiguous indices and canonical discontinuity indexes.
func NormalizeHLS(p *HLSPlaylist) *HLSPlaylist {
	if p.Version == 0 {
		p.Version = 3
	}
	p.Discontinuities = p.Discontinuities[:0]
	for i := range p.Segments {
		p.Segments[i].Index = p.MediaSequence + i
		if p.Segments[i].DiscontinuityBefore {
			p.Discontinuities = append(p.Discontinuities, p.Segments[i].Index)
		}
	}
	return p
}

// SerializeHLS emits canonical deterministic text.
func SerializeHLS(p *HLSPlaylist) []byte {
	NormalizeHLS(p)
	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	fmt.Fprintf(&b, "#EXT-X-VERSION:%d\n", p.Version)
	fmt.Fprintf(&b, "#EXT-X-TARGETDURATION:%d\n", p.TargetDuration)
	fmt.Fprintf(&b, "#EXT-X-MEDIA-SEQUENCE:%d\n", p.MediaSequence)
	for i, s := range p.Segments {
		if s.DiscontinuityBefore {
			b.WriteString("#EXT-X-DISCONTINUITY\n")
		}
		d := strconv.FormatFloat(s.Duration, 'f', 3, 64)
		fmt.Fprintf(&b, "#EXTINF:%s,\n%s\n", d, s.URI)
		_ = i
	}
	if p.EndList {
		b.WriteString("#EXT-X-ENDLIST\n")
	}
	return []byte(b.String())
}

// CompareHLS returns the first semantic mismatch in stable field order.
func CompareHLS(want, got *HLSPlaylist) *Mismatch {
	scalars := []struct {
		f, w, g string
	}{
		{"VERSION", strconv.Itoa(want.Version), strconv.Itoa(got.Version)},
		{"TARGETDURATION", strconv.Itoa(want.TargetDuration), strconv.Itoa(got.TargetDuration)},
		{"MEDIA-SEQUENCE", strconv.Itoa(want.MediaSequence), strconv.Itoa(got.MediaSequence)},
		{"segment.count", strconv.Itoa(len(want.Segments)), strconv.Itoa(len(got.Segments))},
		{"discontinuity.count", strconv.Itoa(len(want.Discontinuities)), strconv.Itoa(len(got.Discontinuities))},
	}
	for _, c := range scalars {
		if c.w != c.g {
			return &Mismatch{Field: c.f, Want: c.w, Got: c.g}
		}
	}
	for i := 0; i < min(len(want.Segments), len(got.Segments)); i++ {
		w, g := want.Segments[i], got.Segments[i]
		prefix := fmt.Sprintf("segment[%d]", i)
		fields := []struct{ f, w, g string }{
			{prefix + ".index", strconv.Itoa(w.Index), strconv.Itoa(g.Index)},
			{prefix + ".uri", w.URI, g.URI},
			{prefix + ".duration", strconv.FormatFloat(w.Duration, 'f', 3, 64), strconv.FormatFloat(g.Duration, 'f', 3, 64)},
			{prefix + ".discontinuity", strconv.FormatBool(w.DiscontinuityBefore), strconv.FormatBool(g.DiscontinuityBefore)},
		}
		for _, c := range fields {
			if c.w != c.g {
				return &Mismatch{Field: c.f, Want: c.w, Got: c.g}
			}
		}
	}
	for i := 0; i < min(len(want.Discontinuities), len(got.Discontinuities)); i++ {
		if want.Discontinuities[i] != got.Discontinuities[i] {
			return &Mismatch{Field: fmt.Sprintf("discontinuity[%d]", i), Want: strconv.Itoa(want.Discontinuities[i]), Got: strconv.Itoa(got.Discontinuities[i])}
		}
	}
	return nil
}

// RoundTripHLS parses, serializes, and reparses a playlist.
func RoundTripHLS(data []byte) (canonical []byte, original, reparsed *HLSPlaylist, mismatch *Mismatch, err error) {
	original, err = ParseHLS(data)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	canonical = SerializeHLS(original)
	reparsed, err = ParseHLS(canonical)
	if err != nil {
		return canonical, original, nil, nil, err
	}
	mismatch = CompareHLS(NormalizeHLS(original), NormalizeHLS(reparsed))
	return canonical, original, reparsed, mismatch, nil
}
