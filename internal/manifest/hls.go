package manifest

import (
	"bytes"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/Eyevinn/hls-m3u8/m3u8"

	"aircheck/internal/media"
)

// Variant is one EXT-X-STREAM-INF entry.
type Variant struct {
	Raw        string
	URI        string
	Bandwidth  uint32
	Codecs     string
	Resolution string
	FrameRate  string
	AudioGroup string
	Line       int
	Offset     int64
}

// Master is a parsed multivariant playlist.
type Master struct {
	Artifact string
	Version  uint8
	Variants []Variant
	Audio    []Variant // EXT-X-MEDIA TYPE=AUDIO renditions referenced by groups
	Lines    []Line
}

// HSLOptions configures media-playlist parsing.
type HSLOptions struct {
	Root  string // input root, for URI containment
	Clock media.Clock
	// MaxDurationSec rejects a target duration above this bound; 0 disables.
	MaxDurationSec float64
}

// ParseHLSMaster decodes a master playlist with the Eyevinn parser and enriches
// each reference with its line and byte offset from a positional scan.
func ParseHLSMaster(art Artifact, opt HSLOptions) (*Master, error) {
	lines := ScanLines(art.Data)
	pl, typ, err := m3u8.Decode(*bytes.NewBuffer(art.Data), true)
	if err != nil {
		return nil, media.WrapFault(media.FaultManifestSyntax, media.SourcePos{Artifact: art.Name}, err)
	}
	if typ != m3u8.MASTER {
		return nil, media.NewFault(media.FaultManifestSyntax, media.SourcePos{Artifact: art.Name},
			"expected a multivariant (master) playlist")
	}
	master := pl.(*m3u8.MasterPlaylist)
	if len(master.Variants) == 0 {
		return nil, media.NewFault(media.FaultManifestSyntax, media.SourcePos{Artifact: art.Name},
			"master playlist has no variants")
	}
	defer master.ReleasePlaylist()

	// EXT-X-STREAM-INF URI lines are the only bare lines in a master playlist.
	uriLines := bareLines(lines)
	if len(uriLines) != len(master.Variants) {
		return nil, media.NewFault(media.FaultManifestSyntax, media.SourcePos{Artifact: art.Name, Line: len(lines)},
			fmt.Sprintf("%d variant URIs but %d EXT-X-STREAM-INF entries", len(uriLines), len(master.Variants)))
	}

	m := &Master{Artifact: art.Name, Version: master.Version(), Lines: lines}
	seen := map[string]int{}
	for i, v := range master.Variants {
		l := uriLines[i]
		clean, err := ValidateURI(opt.Root, v.URI)
		if err != nil {
			return nil, media.WrapFault(media.FaultManifestRange,
				media.SourcePos{Artifact: art.Name, Line: l.Num, Offset: l.Offset}, err)
		}
		if prev, dup := seen[clean]; dup {
			return nil, media.NewFault(media.FaultManifestRange,
				media.SourcePos{Artifact: art.Name, Line: l.Num, Offset: l.Offset},
				fmt.Sprintf("duplicate variant URI %q (first at line %d)", clean, prev))
		}
		seen[clean] = l.Num
		if v.Iframe {
			continue
		}
		m.Variants = append(m.Variants, Variant{
			Raw: v.URI, URI: clean, Bandwidth: v.Bandwidth, Codecs: v.Codecs,
			Resolution: v.Resolution, AudioGroup: v.Audio,
			FrameRate: fmt.Sprintf("%g", v.FrameRate), Line: l.Num, Offset: l.Offset,
		})
	}
	if len(m.Variants) == 0 {
		return nil, media.NewFault(media.FaultManifestSyntax, media.SourcePos{Artifact: art.Name},
			"master playlist has only I-frame variants")
	}

	// Validate EXT-X-MEDIA AUDIO renditions too, without requiring them to be
	// referenced: a bad URI anywhere in the manifest is a range fault.
	for _, alt := range masterAlternativeAudio(master) {
		if alt.URI == "" {
			continue // muxed rendition, no separate manifest
		}
		clean, err := ValidateURI(opt.Root, alt.URI)
		if err != nil {
			return nil, media.WrapFault(media.FaultManifestRange, media.SourcePos{Artifact: art.Name}, err)
		}
		l := findLineContaining(lines, alt.URI)
		m.Audio = append(m.Audio, Variant{
			Raw: alt.URI, URI: clean,
			Line: l.Num, Offset: l.Offset, AudioGroup: alt.GroupId,
		})
	}
	return m, nil
}

// masterAlternativeAudio returns each distinct EXT-X-MEDIA AUDIO rendition
// once. The library attaches the same alternative to every variant that
// references its group, so dedupe by group id and URI.
func masterAlternativeAudio(p *m3u8.MasterPlaylist) []m3u8.Alternative {
	var out []m3u8.Alternative
	seen := map[string]bool{}
	for _, v := range p.Variants {
		for _, a := range v.Alternatives {
			if a.Type != "AUDIO" {
				continue
			}
			key := a.GroupId + "\x00" + a.URI + "\x00" + a.Name
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, *a)
		}
	}
	return out
}

// SelectVariant picks the variant whose bandwidth is closest to targetBps.
func (m *Master) SelectVariant(targetBps int64) (Variant, error) {
	if len(m.Variants) == 0 {
		return Variant{}, media.NewFault(media.FaultManifestSyntax, media.SourcePos{Artifact: m.Artifact},
			"no variants to select")
	}
	best := m.Variants[0]
	bestDelta := int64(1 << 62)
	for _, v := range m.Variants {
		d := int64(v.Bandwidth) - targetBps
		if d < 0 {
			d = -d
		}
		if d < bestDelta {
			best, bestDelta = v, d
		}
	}
	return best, nil
}

// ParseHLSMedia decodes a media playlist. The Eyevinn parser provides the
// tag structure; this function adds exact rational durations, line/offset
// provenance, and the range/duplicate/duration rejections the library leaves
// to callers.
func ParseHLSMedia(art Artifact, opt HSLOptions) (*Media, error) {
	if !opt.Clock.Valid() {
		return nil, fmt.Errorf("manifest: HSLOptions.Clock required")
	}
	lines := ScanLines(art.Data)
	pl, typ, err := m3u8.Decode(*bytes.NewBuffer(art.Data), true)
	if err != nil {
		return nil, media.WrapFault(media.FaultManifestSyntax, media.SourcePos{Artifact: art.Name}, err)
	}
	if typ != m3u8.MEDIA {
		return nil, media.NewFault(media.FaultManifestSyntax, media.SourcePos{Artifact: art.Name},
			"expected a media playlist")
	}
	p := pl.(*m3u8.MediaPlaylist)
	defer p.ReleasePlaylist()

	if p.TargetDuration == 0 {
		return nil, media.NewFault(media.FaultManifestSyntax, media.SourcePos{Artifact: art.Name},
			"EXT-X-TARGETDURATION missing")
	}
	target := opt.Clock.FromTicks(int64(p.TargetDuration) * opt.Clock.TicksPerSecond())
	if opt.MaxDurationSec > 0 && float64(p.TargetDuration) > opt.MaxDurationSec {
		return nil, media.NewFault(media.FaultDurationMismatch, media.SourcePos{Artifact: art.Name},
			fmt.Sprintf("target duration %ds exceeds limit %.1fs", p.TargetDuration, opt.MaxDurationSec))
	}

	out := &Media{
		Artifact:         art.Name,
		Version:          p.Version(),
		TargetDuration:   target,
		TargetSec:        float64(p.TargetDuration),
		SeqStart:         int64(p.SeqNo),
		EndList:          p.Closed,
		DiscontinuitySeq: int64(p.DiscontinuitySeq),
	}

	uriLines := bareLines(lines)
	if len(uriLines) != len(p.Segments[:p.Count()]) {
		return nil, media.NewFault(media.FaultManifestSyntax, media.SourcePos{Artifact: art.Name},
			fmt.Sprintf("%d segment URIs but parser found %d segments", len(uriLines), p.Count()))
	}

	// Resolve EXT-X-MAP at playlist level, then per segment overrides.
	initFor := func(mp *m3u8.Map, pos media.SourcePos) (*SegRef, error) {
		if mp == nil {
			return nil, nil
		}
		clean, err := ValidateURI(opt.Root, mp.URI)
		if err != nil {
			return nil, media.WrapFault(media.FaultManifestRange, pos, err)
		}
		l := findLineContaining(lines, "URI=\""+mp.URI+"\"")
		return &SegRef{URI: clean, Raw: mp.URI, Line: l.Num, Offset: l.Offset, RangeFrom: mp.Offset, RangeLen: mp.Limit}, nil
	}
	topInit, err := initFor(p.Map, media.SourcePos{Artifact: art.Name})
	if err != nil {
		return nil, err
	}
	out.Init = topInit

	seenSeq := map[int64]int{}
	seenID := map[string]int{}
	var tick int64
	for i, s := range p.Segments[:p.Count()] {
		l := uriLines[i]
		pos := media.SourcePos{Artifact: art.Name, Line: l.Num, Offset: l.Offset, Segment: i}
		clean, err := ValidateURI(opt.Root, s.URI)
		if err != nil {
			return nil, media.WrapFault(media.FaultManifestRange, pos, err)
		}
		id := clean
		if s.Limit > 0 {
			id = fmt.Sprintf("%s[%d@%d]", clean, s.Limit, s.Offset)
		}
		if err := duplicateSeq(seenSeq, seenID, int64(s.SeqId), id, l.Num); err != nil {
			return nil, media.WrapFault(media.FaultManifestRange, pos, err)
		}
		dur, err := DurationFromEXTINF(s.Duration, opt.Clock)
		if err != nil {
			return nil, media.WrapFault(media.FaultDurationMismatch, pos, err)
		}
		if dur.Ticks() <= 0 {
			return nil, media.NewFault(media.FaultDurationMismatch, pos,
				fmt.Sprintf("non-positive segment duration %.3fs", s.Duration))
		}
		if opt.MaxDurationSec > 0 && dur.Seconds() > opt.MaxDurationSec {
			return nil, media.NewFault(media.FaultDurationMismatch, pos,
				fmt.Sprintf("segment duration %.3fs exceeds limit %.1fs", dur.Seconds(), opt.MaxDurationSec))
		}
		segInit := topInit
		if s.Map != nil && (p.Map == nil || *s.Map != *p.Map) {
			segInit, err = initFor(s.Map, pos)
			if err != nil {
				return nil, err
			}
		}
		segStart := tick
		tick += dur.Ticks()
		out.Segments = append(out.Segments, Segment{
			Seq: int64(s.SeqId), Ref: SegRef{
				URI: clean, Raw: s.URI, Line: l.Num, Offset: l.Offset,
				RangeFrom: s.Offset, RangeLen: s.Limit,
			},
			Duration: dur, DurationSec: s.Duration, TotalTicks: segStart,
			Discontinuity: s.Discontinuity, Init: segInit,
		})
	}
	if !p.Closed {
		return nil, media.NewFault(media.FaultManifestSyntax, media.SourcePos{Artifact: art.Name},
			"media playlist has no EXT-X-ENDLIST")
	}
	return out, nil
}

// DurationFromEXTINF converts a decimal seconds value to exact ticks. It
// rejects NaN/inf and negative values; exactness uses the decimal digits, not
// the float64 approximation.
func DurationFromEXTINF(sec float64, clock media.Clock) (media.Duration, error) {
	if sec <= 0 || sec != sec || sec > 1e9 {
		return media.Duration{}, fmt.Errorf("invalid EXTINF duration %v", sec)
	}
	// The float64 a manifest author wrote is rarely the value the decoder
	// produced: 0.1 already sits slightly above 1/10, and 0.3333333333333333
	// multiplied by 90000 lands on a whole tick by accident. Decide exactness
	// from the shortest decimal representation, not from float arithmetic, so
	// an author who meant 1/3 s is reported as inexact rather than absorbed.
	text := strconv.FormatFloat(sec, 'f', -1, 64)
	frac := 0
	if i := strings.IndexByte(text, '.'); i >= 0 {
		frac = len(text) - i - 1
	}
	if frac > 0 {
		den := int64(1)
		for j := 0; j < frac; j++ {
			den *= 10
		}
		ipt, fpt := text[:len(text)-frac-1], text[len(text)-frac:]
		ip, err1 := strconv.ParseInt(ipt, 10, 64)
		fp, err2 := strconv.ParseInt(fpt, 10, 64)
		if err1 != nil || err2 != nil {
			return media.Duration{}, fmt.Errorf("EXTINF %q malformed", text)
		}
		d, err := clock.FromRational(ip*den+fp, den)
		if err != nil {
			return media.Duration{}, fmt.Errorf("EXTINF %q is not exact at %d ticks/s", text, clock.TicksPerSecond())
		}
		return d, nil
	}
	return clock.FromTicks(parseWhole(text, clock)), nil
}

// parseWhole parses an integer number of seconds into ticks.
func parseWhole(text string, clock media.Clock) int64 {
	v, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return 0
	}
	return v * clock.TicksPerSecond()
}

// bareLines returns non-empty, non-comment lines in order.
func bareLines(lines []Line) []Line {
	out := make([]Line, 0, len(lines))
	for _, l := range lines {
		t := strings.TrimSpace(l.Text)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		out = append(out, l)
	}
	return out
}

// findLineContaining returns the first line containing needle, or line 1.
func findLineContaining(lines []Line, needle string) Line {
	if needle == "" {
		return Line{Num: 1, Offset: 0}
	}
	for _, l := range lines {
		if strings.Contains(l.Text, needle) {
			return l
		}
	}
	return Line{Num: 1, Offset: 0}
}

// SortVariants orders variants by ascending bandwidth (test helper).
func SortVariants(v []Variant) {
	sort.Slice(v, func(i, j int) bool { return v[i].Bandwidth < v[j].Bandwidth })
}
