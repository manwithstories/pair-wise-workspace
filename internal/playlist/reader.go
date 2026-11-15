package playlist

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Playlist is the generic parse result. Kind is "master" or "media".
type Playlist struct {
	Kind   string
	Master *MasterPlaylist
	Media  *MediaPlaylist
}

func parseAttrValue(line, key string) (string, bool) {
	marker := key + "="
	start := strings.Index(line, marker)
	if start < 0 {
		return "", false
	}
	start += len(marker)
	if start >= len(line) {
		return "", true
	}
	rest := line[start:]
	if rest[0] == '"' {
		end := strings.IndexByte(rest[1:], '"')
		if end < 0 {
			return strings.Trim(rest, `"`), true
		}
		return rest[1 : 1+end], true
	}
	end := strings.IndexAny(rest, ",\n")
	if end < 0 {
		return rest, true
	}
	return rest[:end], true
}

func parseMeta(line string) (MediaEntry, error) {
	var entry MediaEntry
	pairs := strings.Split(line, ",")
	for _, pair := range pairs {
		parts := strings.SplitN(pair, "=", 2)
		if len(parts) != 2 {
			continue
		}
		value, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil {
			return entry, fmt.Errorf("bad meta value %q: %w", pair, err)
		}
		switch parts[0] {
		case "seq":
			entry.Seq = value
		case "ts":
			entry.SourceTSMS = value
		case "meas":
			entry.MeasuredMS = value
		case "decl":
			entry.DeclaredMS = value
		}
	}
	return entry, nil
}

// Parse reads HLS text defensively. Malformed input produces an error instead
// of panicking, so callers can convert parser problems into report failures.
func Parse(data []byte) (*Playlist, error) {
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	header := false
	var media *MediaPlaylist
	var master *MasterPlaylist
	var pending MediaEntry
	var haveExtinf, haveMeta, haveGap bool
	var pendingDuration float64

	flush := func(uri string) error {
		if media == nil {
			return fmt.Errorf("segment URI outside media playlist: %q", uri)
		}
		if !haveExtinf {
			return fmt.Errorf("URI %q is missing EXTINF", uri)
		}
		pending.URI = uri
		pending.DurationS = pendingDuration
		pending.IsGap = haveGap
		media.Entries = append(media.Entries, pending)
		pending, haveExtinf, haveMeta, haveGap, pendingDuration = MediaEntry{}, false, false, false, 0
		return nil
	}

	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if line == "#EXTM3U" {
			header = true
			continue
		}
		if !header {
			return nil, fmt.Errorf("line %d: missing #EXTM3U header", lineNo)
		}
		switch {
		case strings.HasPrefix(line, "#EXT-X-STREAM-INF:"):
			if master == nil {
				master = &MasterPlaylist{Version: 3, FileName: ""}
			}
			ref := MediaRef{}
			if value, ok := parseAttrValue(line, "BANDWIDTH"); ok {
				bandwidth, err := strconv.Atoi(value)
				if err != nil {
					return nil, fmt.Errorf("line %d: bad BANDWIDTH: %w", lineNo, err)
				}
				ref.BandwidthBps = bandwidth
			}
			if value, ok := parseAttrValue(line, "RESOLUTION"); ok {
				ref.Resolution = value
			}
			if value, ok := parseAttrValue(line, "NAME"); ok {
				ref.Name = value
			}
			master.Media = append(master.Media, ref)
		case strings.HasPrefix(line, "#EXT-X-TARGETDURATION:"):
			target, err := strconv.Atoi(strings.TrimPrefix(line, "#EXT-X-TARGETDURATION:"))
			if err != nil {
				return nil, fmt.Errorf("line %d: bad TARGETDURATION: %w", lineNo, err)
			}
			if media == nil {
				media = &MediaPlaylist{TargetDuration: target}
			}
			media.TargetDuration = target
		case strings.HasPrefix(line, "#EXT-X-MEDIA-SEQUENCE:"):
			sequence, err := strconv.ParseInt(strings.TrimPrefix(line, "#EXT-X-MEDIA-SEQUENCE:"), 10, 64)
			if err != nil {
				return nil, fmt.Errorf("line %d: bad MEDIA-SEQUENCE: %w", lineNo, err)
			}
			if media == nil {
				media = &MediaPlaylist{}
			}
			media.MediaSequence = sequence
		case strings.HasPrefix(line, "#EXTM3U8CTL-RENDITION:"):
			rendition, err := strconv.Atoi(strings.TrimPrefix(line, "#EXTM3U8CTL-RENDITION:"))
			if err != nil {
				return nil, fmt.Errorf("line %d: bad rendition: %w", lineNo, err)
			}
			if media == nil {
				media = &MediaPlaylist{}
			}
			media.Rendition = rendition
		case strings.HasPrefix(line, "#EXTM3U8CTL-INFO:"):
			if media == nil {
				media = &MediaPlaylist{}
			}
			info := strings.TrimPrefix(line, "#EXTM3U8CTL-INFO:")
			if value, ok := parseAttrValue(info, "name"); ok {
				media.Name = value
			}
			if value, ok := parseAttrValue(info, "bandwidth"); ok {
				bandwidth, err := strconv.Atoi(value)
				if err != nil {
					return nil, fmt.Errorf("line %d: bad bandwidth: %w", lineNo, err)
				}
				media.BandwidthBps = bandwidth
			}
			if value, ok := parseAttrValue(info, "resolution"); ok {
				media.Resolution = value
			}
		case strings.HasPrefix(line, "#EXTM3U8CTL-TARGET-DURATION:"):
			target, err := strconv.Atoi(strings.TrimPrefix(line, "#EXTM3U8CTL-TARGET-DURATION:"))
			if err != nil {
				return nil, fmt.Errorf("line %d: bad target duration: %w", lineNo, err)
			}
			if master == nil {
				master = &MasterPlaylist{Version: 3}
			}
			master.TargetDuration = target
		case strings.HasPrefix(line, "#EXTINF:"):
			durationText := strings.TrimPrefix(line, "#EXTINF:")
			if comma := strings.IndexByte(durationText, ','); comma >= 0 {
				durationText = durationText[:comma]
			}
			duration, err := strconv.ParseFloat(strings.TrimSpace(durationText), 64)
			if err != nil {
				return nil, fmt.Errorf("line %d: bad EXTINF: %w", lineNo, err)
			}
			haveExtinf, pendingDuration = true, duration
		case strings.HasPrefix(line, "#EXTM3U8CTL-META:"):
			entry, err := parseMeta(strings.TrimPrefix(line, "#EXTM3U8CTL-META:"))
			if err != nil {
				return nil, fmt.Errorf("line %d: %w", lineNo, err)
			}
			pending.Seq, pending.SourceTSMS = entry.Seq, entry.SourceTSMS
			pending.MeasuredMS, pending.DeclaredMS = entry.MeasuredMS, entry.DeclaredMS
			haveMeta = true
		case line == "#EXT-X-GAP":
			haveGap = true
		case strings.HasPrefix(line, "#"):
			// Unknown standard or custom tags are intentionally ignored.
		default:
			if master != nil && len(master.Media) > 0 && media == nil {
				master.Media[len(master.Media)-1].URI = line
				continue
			}
			if err := flush(line); err != nil {
				return nil, fmt.Errorf("line %d: %w", lineNo, err)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("playlist scan failed: %w", err)
	}
	if !header {
		return nil, fmt.Errorf("empty or headerless playlist")
	}
	if media != nil && master != nil && len(master.Media) > 0 {
		return nil, fmt.Errorf("playlist mixes master and media tags")
	}
	if master != nil && len(master.Media) > 0 {
		return &Playlist{Kind: "master", Master: master}, nil
	}
	if media == nil {
		return nil, fmt.Errorf("playlist contains no media or stream tags")
	}
	_ = haveMeta
	return &Playlist{Kind: "media", Media: media}, nil
}

// ParseFile opens path and parses it through Parse.
func ParseFile(path string) (*Playlist, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data)
}
