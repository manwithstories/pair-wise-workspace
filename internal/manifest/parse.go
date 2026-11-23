// Package manifest parses the archive's long-form material manifest: per-track
// media clocks, edit lists, and the non-zero origins typical of broadcast
// captures. Parsing hands the timeline to repair and indexing.
package manifest

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"archive.local/archive/internal/model"
)

// Entry is one media track declared in the manifest.
type Entry struct {
	Line        int
	TrackID     uint32
	Source      string // mp4 file path, relative to the manifest directory
	Timescale   uint32
	OriginTicks int64
	Kind        string // video, audio, data
	Duration    int64  // declared media duration in ticks; -1 when unspecified
	Digest      string // lowercase hex sha256 of the source, "" when unspecified
	Edit        []Edit
}

// Edit is one edit-list run mapping a segment of media onto the presentation.
// A negative MediaTime denotes an empty edit (a gap), as in ISO 14496-12.
type Edit struct {
	Line            int
	MediaTime       int64
	SegmentDuration int64 // in media timescale units
	Rate            int32
}

// Manifest is the parsed material list.
type Manifest struct {
	Path    string
	Dir     string
	Entries []Entry
}

// Timebases returns the clocks declared by the manifest, ready for
// model.NewTimeline.
func (m *Manifest) Timebases() []model.Timebase {
	out := make([]model.Timebase, 0, len(m.Entries))
	for _, e := range m.Entries {
		out = append(out, model.Timebase{
			TrackID:     e.TrackID,
			Timescale:   e.Timescale,
			OriginTicks: e.OriginTicks,
		})
	}
	return out
}

// Timeline validates all clocks and returns the shared timeline.
func (m *Manifest) Timeline() (*model.Timeline, error) {
	return model.NewTimeline(m.Timebases()...)
}

// TrackIDs returns the declared track ids in manifest order.
func (m *Manifest) TrackIDs() []uint32 {
	out := make([]uint32, 0, len(m.Entries))
	for _, e := range m.Entries {
		out = append(out, e.TrackID)
	}
	return out
}

// ParseFile reads a manifest from disk. Paths inside it are resolved against
// the manifest's directory and are not allowed to escape it.
func ParseFile(path string) (*Manifest, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, &model.Error{Code: model.CodeManifestSyntax, File: path, Sample: -1, OffsetBytes: -1,
			Detail: "cannot open manifest", Err: err}
	}
	defer f.Close()
	return Parse(path, f)
}

// Parse reads the line-oriented manifest grammar. Sources are confined to the
// manifest's own directory. Recognised directives:
//
//	track <id> <source> <timescale> <origin> <kind> [duration]
//	edit <track-id> <media-time> <duration> <rate-num> <rate-den>
//	digest <track-id> <sha256>
//	# comments and blank lines are ignored
func Parse(path string, r io.Reader) (*Manifest, error) {
	return parseIn(path, filepath.Dir(path), r)
}

// ParseFileWithRoot reads a manifest whose sources are resolved against root
// rather than the manifest's own directory. It lets the demo keep all
// generated material under one explicit working directory while the manifest
// lives beside it.
func ParseFileWithRoot(path, root string) (*Manifest, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, &model.Error{Code: model.CodeManifestSyntax, File: path, Sample: -1, OffsetBytes: -1,
			Detail: "cannot open manifest", Err: err}
	}
	defer f.Close()
	return parseIn(path, root, f)
}

func parseIn(path, root string, r io.Reader) (*Manifest, error) {
	m := &Manifest{Path: path, Dir: root}
	byID := make(map[uint32]int)
	seenDigest := make(map[uint32]bool)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 4096), 1<<20)
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		fields := strings.Fields(text)
		switch fields[0] {
		case "track":
			if len(fields) < 6 || len(fields) > 7 {
				return nil, m.syntaxError(line, "track needs 5 or 6 fields: id source timescale origin kind [duration]")
			}
			id64, err := strconv.ParseUint(fields[1], 10, 32)
			if err != nil || id64 == 0 {
				return nil, m.syntaxError(line, "invalid track id")
			}
			id := uint32(id64)
			if _, dup := byID[id]; dup {
				return nil, m.referenceError(line, id, "duplicate track id")
			}
			ts64, err := strconv.ParseUint(fields[3], 10, 32)
			if err != nil || ts64 == 0 {
				return nil, m.syntaxError(line, "invalid timescale")
			}
			origin, err := strconv.ParseInt(fields[4], 10, 64)
			if err != nil {
				return nil, m.syntaxError(line, "invalid origin tick")
			}
			kind := fields[5]
			if kind != "video" && kind != "audio" && kind != "data" {
				return nil, m.syntaxError(line, "kind must be video, audio or data")
			}
			dur := int64(-1)
			if len(fields) == 7 {
				dur, err = strconv.ParseInt(fields[6], 10, 64)
				if err != nil || dur < 0 {
					return nil, m.syntaxError(line, "invalid duration")
				}
			}
			src := fields[2]
			if !strings.HasSuffix(strings.ToLower(src), ".mp4") && !strings.HasSuffix(strings.ToLower(src), ".mov") {
				return nil, m.referenceError(line, id, "source must be .mp4 or .mov")
			}
			if _, err := m.resolve(line, id, src); err != nil {
				return nil, err
			}
			byID[id] = len(m.Entries)
			m.Entries = append(m.Entries, Entry{Line: line, TrackID: id, Source: src,
				Timescale: uint32(ts64), OriginTicks: origin, Kind: kind, Duration: dur})
		case "edit":
			if len(fields) != 6 {
				return nil, m.syntaxError(line, "edit needs 5 fields: track media-time duration rate-num rate-den")
			}
			id64, err := strconv.ParseUint(fields[1], 10, 32)
			if err != nil {
				return nil, m.syntaxError(line, "invalid track id")
			}
			idx, ok := byID[uint32(id64)]
			if !ok {
				return nil, m.referenceError(line, uint32(id64), "edit refers to an undeclared track")
			}
			media, err := strconv.ParseInt(fields[2], 10, 64)
			if err != nil {
				return nil, m.syntaxError(line, "invalid media time")
			}
			dur, err := strconv.ParseInt(fields[3], 10, 64)
			if err != nil || dur < 0 {
				return nil, m.syntaxError(line, "invalid segment duration")
			}
			num, err := strconv.ParseInt(fields[4], 10, 32)
			if err != nil {
				return nil, m.syntaxError(line, "invalid rate numerator")
			}
			den, err := strconv.ParseInt(fields[5], 10, 32)
			if err != nil || den == 0 {
				return nil, m.syntaxError(line, "invalid rate denominator")
			}
			// Rate is stored as a 16.16 fixed point value.
			rate := int32(num * 65536 / den)
			e := &m.Entries[idx]
			e.Edit = append(e.Edit, Edit{Line: line, MediaTime: media, SegmentDuration: dur, Rate: rate})
		case "digest":
			if len(fields) != 3 {
				return nil, m.syntaxError(line, "digest needs 2 fields: track sha256")
			}
			id64, err := strconv.ParseUint(fields[1], 10, 32)
			if err != nil {
				return nil, m.syntaxError(line, "invalid track id")
			}
			idx, ok := byID[uint32(id64)]
			if !ok {
				return nil, m.referenceError(line, uint32(id64), "digest refers to an undeclared track")
			}
			hex := strings.ToLower(fields[2])
			if len(hex) != 64 || !isHex(hex) {
				return nil, m.syntaxError(line, "sha256 must be 64 hexadecimal characters")
			}
			if seenDigest[uint32(id64)] {
				return nil, m.referenceError(line, uint32(id64), "duplicate digest")
			}
			seenDigest[uint32(id64)] = true
			m.Entries[idx].Digest = hex
		default:
			return nil, m.syntaxError(line, fmt.Sprintf("unknown directive %q", fields[0]))
		}
	}
	if err := sc.Err(); err != nil {
		return nil, &model.Error{Code: model.CodeManifestSyntax, File: path, Sample: -1, OffsetBytes: -1,
			Detail: "reading manifest", Err: err}
	}
	if len(m.Entries) == 0 {
		return nil, m.syntaxError(line, "manifest declares no tracks")
	}
	if err := m.validate(); err != nil {
		return nil, err
	}
	return m, nil
}
