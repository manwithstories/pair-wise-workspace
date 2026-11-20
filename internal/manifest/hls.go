package manifest

import (
	"bytes"
	"strconv"
	"strings"

	"mediamap/internal/clock"
	"mediamap/internal/fault"
)

// Variant is one entry of an HLS master playlist.
type Variant struct {
	ID        string `json:"id"`
	Bandwidth int    `json:"bandwidth"`
	URI       string `json:"uri"`
}

// ParseHLSMaster parses a master playlist into its variant list. Lines
// that cannot be understood are reported with their byte offset and
// skipped; a playlist with no usable variant is a fatal fault.
func ParseHLSMaster(track string, data []byte) ([]Variant, fault.List) {
	var faults fault.List
	variants := []Variant{}
	var pending *Variant
	for _, ln := range splitLines(data) {
		s := string(ln.text)
		switch {
		case strings.HasPrefix(s, "#EXT-X-STREAM-INF:"):
			v := Variant{ID: strconv.Itoa(len(variants))}
			for _, attr := range strings.Split(s[len("#EXT-X-STREAM-INF:"):], ",") {
				k, val, ok := strings.Cut(attr, "=")
				if !ok {
					continue
				}
				if k == "BANDWIDTH" {
					if n, err := strconv.Atoi(val); err == nil {
						v.Bandwidth = n
					}
				}
			}
			pending = &v
		case s == "" || strings.HasPrefix(s, "#"):
			// comments, tags we do not consume
		default:
			if pending == nil {
				faults = append(faults, fault.New(fault.StageManifest, track, ln.offset, true,
					"variant URI %q without preceding EXT-X-STREAM-INF", s))
				continue
			}
			pending.URI = s
			variants = append(variants, *pending)
			pending = nil
		}
	}
	if len(variants) == 0 {
		faults = append(faults, fault.New(fault.StageManifest, track, fault.NoOffset, false,
			"master playlist has no usable variant"))
	}
	return variants, faults
}

// ParseHLSMedia parses a media playlist into one representation. A
// segment URI without a preceding EXTINF gets its boundary derived from
// EXT-X-TARGETDURATION and is flagged derived; the playlist never invents
// URIs and never reorders them.
func ParseHLSMedia(track, repID string, data []byte) (Representation, fault.List) {
	rep := Representation{ID: repID}
	faults := fault.List{}
	var target clock.Rational
	var pendingDur *clock.Rational
	var pendingOff int64
	var cursor int64 // 90 kHz ticks
	for _, ln := range splitLines(data) {
		s := string(ln.text)
		switch {
		case strings.HasPrefix(s, "#EXT-X-TARGETDURATION:"):
			r, err := parseDecimal(s[len("#EXT-X-TARGETDURATION:"):])
			if err != nil {
				faults = append(faults, fault.New(fault.StageManifest, track, ln.offset, true,
					"malformed TARGETDURATION %q", s))
				break
			}
			target = r
		case strings.HasPrefix(s, "#EXT-X-START:"):
			for _, attr := range strings.Split(s[len("#EXT-X-START:"):], ",") {
				k, val, ok := strings.Cut(attr, "=")
				if !ok || k != "TIME-OFFSET" {
					continue
				}
				r, err := parseDecimal(val)
				if err != nil {
					faults = append(faults, fault.New(fault.StageManifest, track, ln.offset, true,
						"malformed EXT-X-START %q", s))
					break
				}
				rep.StartOffset = secondsToTicks(r)
			}
		case strings.HasPrefix(s, "#EXTINF:"):
			d := strings.TrimSuffix(s[len("#EXTINF:"):], ",")
			r, err := parseDecimal(d)
			if err != nil {
				faults = append(faults, fault.New(fault.StageManifest, track, ln.offset, true,
					"malformed EXTINF %q", s))
				break
			}
			pendingDur = &r
			pendingOff = ln.offset
		case s == "" || strings.HasPrefix(s, "#"):
			// other tags (ENDLIST, MEDIA-SEQUENCE, ...) do not move boundaries
		default:
			seg := SegmentRef{URI: s, Start: cursor}
			switch {
			case pendingDur != nil:
				seg.Duration = secondsToTicks(*pendingDur)
				pendingDur = nil
			case target.Num > 0:
				seg.Duration = secondsToTicks(target)
				seg.Derived = true
				faults = append(faults, fault.New(fault.StageManifest, track, ln.offset, true,
					"segment %q missing EXTINF; duration derived from target", s))
			default:
				faults = append(faults, fault.New(fault.StageManifest, track, ln.offset, false,
					"segment %q has no EXTINF and no TARGETDURATION", s))
				continue
			}
			rep.Segments = append(rep.Segments, seg)
			cursor += seg.Duration
		}
	}
	if pendingDur != nil {
		faults = append(faults, fault.New(fault.StageManifest, track, pendingOff, true,
			"trailing EXTINF without segment URI"))
	}
	if len(rep.Segments) == 0 {
		faults = append(faults, fault.New(fault.StageManifest, track, fault.NoOffset, false,
			"media playlist has no segments"))
	}
	return rep, faults
}

type line struct {
	text   []byte
	offset int64
}

// splitLines splits a playlist into lines with their byte offsets.
func splitLines(data []byte) []line {
	var out []line
	var off int64
	for _, raw := range bytes.SplitAfter(data, []byte("\n")) {
		text := bytes.TrimRight(raw, "\r\n")
		if len(text) > 0 {
			out = append(out, line{text: text, offset: off})
		}
		off += int64(len(raw))
	}
	return out
}
