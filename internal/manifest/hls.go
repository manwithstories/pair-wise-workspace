package manifest

import (
	"bufio"
	"bytes"
	"fmt"
	"strconv"
	"strings"
	"time"

	"avlab/internal/media"
)

// HLS avlab bundle extensions. Standard tags carry header, duration and
// discontinuity information; the custom tag below carries the per-representation
// URLs of one aligned segment so all bitrate levels share one playlist.
const (
	tagM3U8          = "#EXTM3U"
	tagTarget        = "#EXT-X-TARGETDURATION:"
	tagVersion       = "#EXT-X-VERSION:"
	tagIndependent   = "#EXT-X-INDEPENDENT-SEGMENTS"
	tagRep           = "#EXT-X-AVLAB-REP:"
	tagMinBuffer     = "#EXT-X-AVLAB-MIN-BUFFER-TICKS:"
	tagSegment       = "#EXT-X-AVLAB-SEGMENT:"
	tagExtinf        = "#EXTINF:"
	tagDiscontinuity = "#EXT-X-DISCONTINUITY"
	tagEndList       = "#EXT-X-ENDLIST"
)

// ParseHLS decodes an avlab HLS playlist bundle into the normalized Manifest.
func ParseHLS(data []byte) (*Manifest, error) {
	if err := checkSize(len(data)); err != nil {
		return nil, err
	}
	m := &Manifest{Kind: KindHLS}
	repIndex := map[string]int{}
	var epoch media.Epoch
	var segIndex uint32 = 1
	var pendingDisco bool
	var pendingDur media.MediaTime = -1
	haveM3U8, haveEnd := false, false
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), media.MaxManifestBytes)
	for lineNo := 1; sc.Scan(); lineNo++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		switch {
		case line == tagM3U8:
			haveM3U8 = true
		case line == tagIndependent || strings.HasPrefix(line, tagVersion):
			// informational, nothing to project
		case strings.HasPrefix(line, tagTarget):
			secs, err := strconv.Atoi(strings.TrimPrefix(line, tagTarget))
			if err != nil || secs <= 0 {
				return nil, damagedf(lineNo, "invalid target duration")
			}
			m.TargetTick = media.FromDuration(time.Duration(secs) * time.Second)
		case strings.HasPrefix(line, tagRep):
			if err := parseRepLine(line, m, repIndex); err != nil {
				return nil, err
			}
		case strings.HasPrefix(line, tagMinBuffer):
			ticks, err := strconv.ParseInt(
				strings.TrimPrefix(line, tagMinBuffer), 10, 64)
			if err != nil || ticks < 0 {
				return nil, damagedf(lineNo, "invalid min-buffer ticks")
			}
			m.MinBufferTick = media.MediaTime(ticks)
		case line == tagDiscontinuity:
			pendingDisco = true
		case strings.HasPrefix(line, tagExtinf):
			dur, err := parseExtinf(line, lineNo)
			if err != nil {
				return nil, err
			}
			pendingDur = dur
		case strings.HasPrefix(line, tagSegment):
			dur, err := consumeDur(&pendingDur, lineNo)
			if err != nil {
				return nil, err
			}
			if pendingDisco {
				epoch++
				pendingDisco = false
			}
			if err := appendSegmentRow(line, m, repIndex, &segIndex, epoch, dur); err != nil {
				return nil, err
			}
		case line == tagEndList:
			haveEnd = true
		default:
			return nil, damagedf(lineNo, "unrecognized playlist line: "+line)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, media.WrapFault(media.CodeManifestDamaged, false, "scan failed", err)
	}
	if !haveM3U8 {
		return nil, media.NewFault(media.CodeManifestDamaged, false, "missing #EXTM3U header")
	}
	if !haveEnd {
		return nil, media.NewFault(media.CodeManifestDamaged, false, "missing #EXT-X-ENDLIST")
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return m, nil
}

func parseRepLine(line string, m *Manifest, idx map[string]int) error {
	attrs, err := parseAttrs(strings.TrimPrefix(line, tagRep))
	if err != nil {
		return media.WrapFault(media.CodeManifestDamaged, false, "bad rep attributes", err)
	}
	id := attrs["ID"]
	if id == "" {
		return media.NewFault(media.CodeManifestDamaged, false, "rep without ID")
	}
	if _, dup := idx[id]; dup {
		return media.NewFault(media.CodeManifestDamaged, false, "duplicate rep id: "+id)
	}
	bw, _ := strconv.ParseUint(attrs["BANDWIDTH"], 10, 32)
	var w, h uint32
	if res := attrs["RESOLUTION"]; res != "" {
		parts := strings.Split(res, "x")
		if len(parts) != 2 {
			return media.NewFault(media.CodeManifestDamaged, false, "bad RESOLUTION: "+res)
		}
		nw, e1 := strconv.ParseUint(parts[0], 10, 32)
		nh, e2 := strconv.ParseUint(parts[1], 10, 32)
		if e1 != nil || e2 != nil {
			return media.NewFault(media.CodeManifestDamaged, false, "bad RESOLUTION: "+res)
		}
		w, h = uint32(nw), uint32(nh)
	}
	idx[id] = len(m.Representations)
	m.Representations = append(m.Representations, Representation{
		ID: id, Bandwidth: uint32(bw), Width: w, Height: h, MIME: attrs["MIME"]})
	return nil
}

func parseExtinf(line string, lineNo int) (media.MediaTime, error) {
	rest := strings.TrimPrefix(line, tagExtinf)
	if comma := strings.Index(rest, ","); comma >= 0 {
		rest = rest[:comma]
	}
	secs, err := strconv.ParseFloat(strings.TrimSpace(rest), 64)
	if err != nil || secs <= 0 {
		return 0, damagedf(lineNo, "invalid EXTINF duration")
	}
	return media.FromDuration(time.Duration(secs * float64(time.Second))), nil
}

func consumeDur(pending *media.MediaTime, lineNo int) (media.MediaTime, error) {
	if *pending < 0 {
		return 0, damagedf(lineNo, "segment row without preceding EXTINF")
	}
	d := *pending
	*pending = -1
	return d, nil
}

func appendSegmentRow(line string, m *Manifest, idx map[string]int,
	segIndex *uint32, epoch media.Epoch, dur media.MediaTime) error {
	rows := strings.Split(strings.TrimPrefix(line, tagSegment), ",")
	seen := map[string]bool{}
	startTickByRep := make([]media.MediaTime, len(m.Representations))
	for ri := range m.Representations {
		if len(m.Representations[ri].Segments) > 0 {
			last := m.Representations[ri].Segments[len(m.Representations[ri].Segments)-1]
			startTickByRep[ri] = last.StartTick + last.DurationTick
		}
	}
	for _, row := range rows {
		kv := strings.SplitN(strings.TrimSpace(row), "=", 2)
		if len(kv) != 2 {
			return media.NewFault(media.CodeManifestDamaged, false, "malformed segment row: "+row)
		}
		ri, ok := idx[kv[0]]
		if !ok {
			return media.NewFault(media.CodeManifestDamaged, false,
				"segment row references undeclared rep: "+kv[0])
		}
		seen[kv[0]] = true
		disco := epoch > 0 && len(m.Representations[ri].Segments) > 0 &&
			m.Representations[ri].Segments[len(m.Representations[ri].Segments)-1].Epoch != epoch
		m.Representations[ri].Segments = append(m.Representations[ri].Segments, Segment{
			Index: *segIndex, URL: kv[1], StartTick: startTickByRep[ri],
			DurationTick: dur, Epoch: epoch, Discontinuity: disco, Keyframe: true})
	}
	if len(seen) != len(m.Representations) {
		return media.NewFault(media.CodeManifestDamaged, false,
			"segment row missing one or more representations")
	}
	*segIndex++
	return nil
}

// RenderHLS serializes a normalized Manifest to a deterministic avlab playlist.
func RenderHLS(m *Manifest) ([]byte, error) {
	if m == nil || m.Kind != KindHLS {
		return nil, media.NewFault(media.CodeInvalidArgument, false,
			"RenderHLS requires an HLS manifest")
	}
	var b bytes.Buffer
	fmt.Fprintln(&b, tagM3U8)
	fmt.Fprintln(&b, tagVersion+"7")
	fmt.Fprintf(&b, "%s%d\n", tagTarget,
		int(m.TargetTick.Duration().Seconds()+0.5))
	fmt.Fprintln(&b, tagIndependent)
	fmt.Fprintf(&b, "%s%d\n", tagMinBuffer, m.MinBufferTick)
	for _, r := range m.Representations {
		fmt.Fprintf(&b, "%sID=%s,BANDWIDTH=%d,RESOLUTION=%dx%d,MIME=%s\n",
			tagRep, r.ID, r.Bandwidth, r.Width, r.Height, r.MIME)
	}
	n := len(m.Representations[0].Segments)
	for si := 0; si < n; si++ {
		s0 := m.Representations[0].Segments[si]
		if s0.Discontinuity {
			fmt.Fprintln(&b, tagDiscontinuity)
		}
		fmt.Fprintf(&b, "%s%.3f,\n", tagExtinf, s0.DurationTick.Milliseconds()/1000)
		var rows []string
		for _, r := range m.Representations {
			rows = append(rows, r.ID+"="+r.Segments[si].URL)
		}
		fmt.Fprintf(&b, "%s%s\n", tagSegment, strings.Join(rows, ","))
	}
	fmt.Fprintln(&b, tagEndList)
	return b.Bytes(), nil
}

func parseAttrs(s string) (map[string]string, error) {
	out := map[string]string{}
	var key, val strings.Builder
	inQuote := false
	flush := func() error {
		if key.Len() == 0 {
			return fmt.Errorf("empty attribute key")
		}
		out[key.String()] = val.String()
		key.Reset()
		val.Reset()
		return nil
	}
	const none, inKey, inVal = 0, 1, 2
	state := none
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"':
			inQuote = !inQuote
		case c == '=' && !inQuote:
			state = inVal
		case c == ',' && !inQuote:
			if err := flush(); err != nil {
				return nil, err
			}
			state = none
		default:
			if state == inVal {
				val.WriteByte(c)
			} else {
				if c == ' ' && state == none {
					continue
				}
				state = inKey
				key.WriteByte(c)
			}
		}
	}
	if inQuote {
		return nil, fmt.Errorf("unterminated quoted attribute")
	}
	if err := flush(); err != nil {
		return nil, err
	}
	return out, nil
}

func damagedf(lineNo int, msg string) *media.Fault {
	return media.NewFault(media.CodeManifestDamaged, false,
		fmt.Sprintf("line %d: %s", lineNo, msg))
}
