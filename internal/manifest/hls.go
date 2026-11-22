package manifest

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"mediad/internal/clock"
	"mediad/internal/fault"
)

// ParseHLS parses an HLS media playlist. It always returns whatever segments
// parsed successfully; structural problems are reported as faults and parsing
// continues so valid segments remain usable.
func ParseHLS(r io.Reader, source string) (*Manifest, *fault.List, error) {
	if r == nil {
		return nil, nil, errors.New("manifest: nil HLS reader")
	}
	base := clock.Default()
	m := &Manifest{Kind: KindHLS}
	rep := Representation{ID: "hls", NativeTimescale: base.Timescale}
	var faults fault.List
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	lineNo := 0
	sawHeader := false
	pendingDuration := int64(0)
	pendingDurationKnown := false
	pendingTitle := ""
	pendingRange := (*Range)(nil)
	pendingMap := ""
	pendingMapRange := (*Range)(nil)
	pendingDiscontinuity := false
	var currentKey *Key
	var startTicks int64
	var seq uint64
	firstSeqSet := false

	parseFault := func(detail string) {
		faults.Add(fault.MustNew(fault.KindManifestParse,
			fault.Location{Segment: source, Offset: int64(lineNo)}, detail, true, false))
	}
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if !sawHeader {
			if line != "#EXTM3U" {
				return nil, &faults, fmt.Errorf("manifest: HLS missing #EXTM3U at %s:%d", source, lineNo)
			}
			sawHeader = true
			continue
		}
		if strings.HasPrefix(line, "#") {
			tag, value, _ := strings.Cut(line, ":")
			switch tag {
			case "#EXT-X-VERSION":
				if v, err := strconv.Atoi(value); err == nil {
					m.Version = v
				} else {
					parseFault("invalid EXT-X-VERSION")
				}
			case "#EXT-X-TARGETDURATION":
				if v, err := clock.ParseDecimalTicks(value, base.Timescale); err == nil {
					m.TargetDuration = v
				} else {
					parseFault("invalid EXT-X-TARGETDURATION")
				}
			case "#EXT-X-MEDIA-SEQUENCE":
				v, err := strconv.ParseUint(value, 10, 64)
				if err != nil {
					parseFault("invalid EXT-X-MEDIA-SEQUENCE")
					break
				}
				m.MediaSequence = v
				if !firstSeqSet && len(rep.Segments) == 0 {
					seq = v
					firstSeqSet = true
				}
			case "#EXT-X-PLAYLIST-TYPE":
				m.PlaylistType = value
			case "#EXT-X-ENDLIST":
				m.EndList = true
			case "#EXTINF":
				durText, title, _ := strings.Cut(value, ",")
				dur, err := clock.ParseDecimalTicks(durText, base.Timescale)
				if err != nil {
					parseFault("invalid EXTINF duration")
				} else {
					pendingDuration = dur
					pendingDurationKnown = true
				}
				pendingTitle = title
			case "#EXT-X-BYTERANGE":
				var prevEnd int64 = -1
				if prev := lastSegment(rep.Segments); prev != nil && prev.ByteRange != nil {
					prevEnd = prev.ByteRange.Offset + prev.ByteRange.Length
				}
				rng, err := parseRange(value, offsetPtr(prevEnd))
				if err != nil {
					parseFault("invalid EXT-X-BYTERANGE")
				} else {
					pendingRange = rng
				}
			case "#EXT-X-MAP":
				attrs, err := parseAttrs(value)
				if err != nil {
					parseFault("invalid EXT-X-MAP")
					break
				}
				pendingMap = attrs["URI"]
				rep.InitURI = pendingMap
				if br, ok := attrs["BYTERANGE"]; ok && br != "" {
					var off int64 = -1
					if prev := lastSegment(rep.Segments); prev != nil && prev.ByteRange != nil {
						off = prev.ByteRange.Offset + prev.ByteRange.Length
					}
					rng, err := parseRange(br, offsetPtr(off))
					if err != nil {
						parseFault("invalid EXT-X-MAP BYTERANGE")
					} else {
						pendingMapRange = rng
					}
				}
			case "#EXT-X-KEY":
				attrs, err := parseAttrs(value)
				if err != nil {
					parseFault("invalid EXT-X-KEY")
					break
				}
				k := Key{
					Method: attrs["METHOD"], URI: attrs["URI"], IV: attrs["IV"],
					KeyFormat: attrs["KEYFORMAT"], KeyFormatVersions: attrs["KEYFORMATVERSIONS"],
				}
				if k.Method == "NONE" {
					currentKey = nil
				} else {
					currentKey = &k
				}
			case "#EXT-X-DISCONTINUITY":
				pendingDiscontinuity = true
			default:
				// Unknown tags are outside the modelled field set.
			}
			continue
		}

		// URI line: emit one segment record.
		seg := Segment{
			URI:              line,
			InitURI:          pendingMap,
			RepresentationID: rep.ID,
			Sequence:         seq,
			Index:            len(rep.Segments),
			Start:            startTicks,
			Duration:         pendingDuration,
			NativeTimescale:  base.Timescale,
			NativeStart:      startTicks,
			NativeDuration:   pendingDuration,
			DurationKnown:    pendingDurationKnown,
			Discontinuity:    pendingDiscontinuity,
			ByteRange:        pendingRange,
			MapURI:           pendingMap,
			MapByteRange:     pendingMapRange,
			Key:              cloneKey(currentKey),
		}
		if !pendingDurationKnown {
			parseFault("segment without EXTINF duration; duration left unknown")
		}
		rep.Segments = append(rep.Segments, seg)
		startTicks += pendingDuration
		seq++
		pendingDuration, pendingDurationKnown, pendingTitle = 0, false, ""
		pendingRange, pendingMapRange, pendingDiscontinuity = nil, nil, false
		_ = pendingTitle
	}
	if err := sc.Err(); err != nil {
		return nil, &faults, fmt.Errorf("manifest: read HLS %s: %w", source, err)
	}
	if !sawHeader {
		return nil, &faults, fmt.Errorf("manifest: empty HLS document %s", source)
	}
	if len(rep.Segments) == 0 {
		faults.Add(fault.MustNew(fault.KindManifestParse,
			fault.Location{Segment: source, Offset: -1}, "playlist has no media segments", true, false))
	}
	m.Representations = []Representation{rep}
	return m, &faults, nil
}
