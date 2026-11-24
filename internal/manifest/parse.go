// Package manifest parses the line-oriented material manifest, edit list and
// track selection rules. Every parsed record keeps its source line number so
// validation errors point at the offending manifest line.
package manifest

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"

	"mediadeliver/internal/model"
	"mediadeliver/internal/timeline"
)

// Material declares one source file and its declared tracks.
type Material struct {
	Line   int
	ID     string
	Path   string
	Format string // "mp4" or "rtp"
	Tracks []TrackSpec
}

// TrackSpec is a TRACK declaration inside a material.
type TrackSpec struct {
	Line  int
	ID    uint32
	Kind  model.TrackKind
	Codec string
}

// EditEntry selects one time range of one source track for a segment.
type EditEntry struct {
	Line       int
	Segment    string
	MaterialID string
	TrackID    uint32
	Start      timeline.Rat // exact seconds
	Duration   timeline.Rat // exact seconds
	Timescale  int64        // ticks per second used by the source track
}

// StartTicks / EndTicks expose the range in source timescale ticks.
func (e *EditEntry) StartTicks() (int64, error) { return e.Start.ScaleTo(e.Timescale) }

// EndTicks returns start+duration in source ticks, checked for overflow.
func (e *EditEntry) EndTicks() (int64, error) {
	end, err := e.Start.Add(e.Duration)
	if err != nil {
		return 0, err
	}
	return end.ScaleTo(e.Timescale)
}

// MappingRule assigns one output slot of a segment to a source track.
type MappingRule struct {
	Line       int
	Segment    string
	Kind       model.TrackKind
	MaterialID string
	TrackID    uint32
}

// Manifest is the fully parsed and validated delivery description.
type Manifest struct {
	Materials []*Material
	Edits     []*EditEntry
	Mappings  []*MappingRule
	// SegmentOrder lists segment names in first-appearance order.
	SegmentOrder []string
}

const op = "parse manifest"

func fail(line int, format string, args ...any) error {
	return model.At(model.KindInvalidInput, "manifest", op, int64(line),
		fmt.Sprintf(format, args...))
}

// Parse reads a manifest. Syntax, one directive per line ('#' starts comments):
//
//	MATERIAL <id> <path> <mp4|rtp>
//	TRACK <material-id> <track-id> <video|audio> <codec>
//	EDIT <segment> <material-id> <track-id> <startTicks> <durationTicks> <timescale>
//	MAP <segment> <video|audio> <material-id> <track-id>
func Parse(r io.Reader) (*Manifest, error) {
	m := &Manifest{}
	mats := map[string]*Material{}
	segSeen := map[string]bool{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		f := strings.Fields(text)
		var err error
		switch f[0] {
		case "MATERIAL":
			err = m.parseMaterial(f, line, mats)
		case "TRACK":
			err = m.parseTrack(f, line, mats)
		case "EDIT":
			err = m.parseEdit(f, line, mats, segSeen)
		case "MAP":
			err = m.parseMap(f, line, mats, segSeen)
		default:
			err = fail(line, "unknown directive %q", f[0])
		}
		if err != nil {
			return nil, err
		}
	}
	if err := sc.Err(); err != nil {
		return nil, model.Wrap(model.KindIO, "manifest", op, int64(line), "read failed", err)
	}
	if len(m.Materials) == 0 {
		return nil, fail(line, "manifest contains no MATERIAL directive")
	}
	if err := m.crossValidate(); err != nil {
		return nil, err
	}
	return m, nil
}

func (m *Manifest) parseMaterial(f []string, line int, mats map[string]*Material) error {
	if len(f) != 4 {
		return fail(line, "MATERIAL needs 3 fields: id path format")
	}
	id, path, format := f[1], f[2], f[3]
	if id == "" || path == "" {
		return fail(line, "MATERIAL id and path must be non-empty")
	}
	if format != "mp4" && format != "rtp" {
		return fail(line, "MATERIAL format must be mp4 or rtp, got %q", format)
	}
	if _, dup := mats[id]; dup {
		return model.At(model.KindConflict, "manifest", op, int64(line),
			"duplicate material id "+id)
	}
	mat := &Material{Line: line, ID: id, Path: path, Format: format}
	mats[id] = mat
	m.Materials = append(m.Materials, mat)
	return nil
}

func (m *Manifest) parseTrack(f []string, line int, mats map[string]*Material) error {
	if len(f) != 5 {
		return fail(line, "TRACK needs 4 fields: material id kind codec")
	}
	mat, ok := mats[f[1]]
	if !ok {
		return fail(line, "TRACK references unknown material %q", f[1])
	}
	id, err := strconv.ParseUint(f[2], 10, 32)
	if err != nil || id == 0 {
		return fail(line, "TRACK id must be a positive uint32, got %q", f[2])
	}
	kind, err := model.ParseTrackKind(f[3])
	if err != nil {
		return model.At(model.KindInvalidInput, "manifest", op, int64(line), err.Error())
	}
	for _, t := range mat.Tracks {
		if t.ID == uint32(id) {
			return model.At(model.KindConflict, "manifest", op, int64(line),
				fmt.Sprintf("duplicate track %d in material %s", id, mat.ID))
		}
	}
	mat.Tracks = append(mat.Tracks, TrackSpec{
		Line: line, ID: uint32(id), Kind: kind, Codec: f[4],
	})
	return nil
}

func (m *Manifest) parseEdit(f []string, line int, mats map[string]*Material, segSeen map[string]bool) error {
	if len(f) != 7 {
		return fail(line, "EDIT needs 6 fields: segment material track start duration timescale")
	}
	mat, track, err := m.resolveTrack(f[2], f[3], line, mats)
	if err != nil {
		return err
	}
	start, err := parseInt64(f[4], line, "start")
	if err != nil {
		return err
	}
	dur, err := parseInt64(f[5], line, "duration")
	if err != nil {
		return err
	}
	ts, err := parseInt64(f[6], line, "timescale")
	if err != nil {
		return err
	}
	if ts <= 0 {
		return model.At(model.KindInvalidInput, "manifest", op, int64(line),
			fmt.Sprintf("illegal time base: timescale %d must be positive", ts))
	}
	startRat, err := timeline.NewRat(start, ts)
	if err != nil {
		return model.Wrap(model.KindInvalidInput, "manifest", op, int64(line), "bad start", err)
	}
	if start < 0 {
		return fail(line, "edit start must be non-negative, got %d", start)
	}
	durRat, err := timeline.NewRat(dur, ts)
	if err != nil {
		return model.Wrap(model.KindInvalidInput, "manifest", op, int64(line), "bad duration", err)
	}
	if err := timeline.CheckNonNegative(durRat, "edit duration"); err != nil {
		return model.At(model.KindInvalidInput, "manifest", op, int64(line), err.Error())
	}
	// Overflow check: start+duration must be representable.
	if _, err := startRat.Add(durRat); err != nil {
		return model.Wrap(model.KindOverflow, "manifest", op, int64(line),
			"edit end overflows", err)
	}
	e := &EditEntry{
		Line: line, Segment: f[1], MaterialID: mat.ID, TrackID: track.ID,
		Start: startRat, Duration: durRat, Timescale: ts,
	}
	if err := m.checkEditOverlap(e, line); err != nil {
		return err
	}
	if !segSeen[e.Segment] {
		segSeen[e.Segment] = true
		m.SegmentOrder = append(m.SegmentOrder, e.Segment)
	}
	m.Edits = append(m.Edits, e)
	return nil
}

func (m *Manifest) checkEditOverlap(e *EditEntry, line int) error {
	for _, prev := range m.Edits {
		if prev.Segment != e.Segment || prev.MaterialID != e.MaterialID ||
			prev.TrackID != e.TrackID {
			continue
		}
		end, err := e.Start.Add(e.Duration)
		if err != nil {
			return err
		}
		prevEnd, err := prev.Start.Add(prev.Duration)
		if err != nil {
			return err
		}
		// Touching ranges (end == next start) are allowed; overlap is not.
		if e.Start.Cmp(prevEnd) < 0 && prev.Start.Cmp(end) < 0 {
			return model.At(model.KindConflict, "manifest", op, int64(line),
				fmt.Sprintf("duplicate/overlapping edit interval on %s track %d (first at line %d)",
					e.MaterialID, e.TrackID, prev.Line))
		}
	}
	return nil
}

func (m *Manifest) parseMap(f []string, line int, mats map[string]*Material, segSeen map[string]bool) error {
	if len(f) != 5 {
		return fail(line, "MAP needs 4 fields: segment kind material track")
	}
	kind, err := model.ParseTrackKind(f[2])
	if err != nil {
		return model.At(model.KindInvalidInput, "manifest", op, int64(line), err.Error())
	}
	mat, track, err := m.resolveTrack(f[3], f[4], line, mats)
	if err != nil {
		return err
	}
	if track.Kind != kind {
		return fail(line, "MAP kind %s does not match track %d kind %s",
			kind, track.ID, track.Kind)
	}
	seg := f[1]
	for _, mp := range m.Mappings {
		if mp.Segment == seg && mp.Kind == kind {
			return model.At(model.KindConflict, "manifest", op, int64(line),
				fmt.Sprintf("duplicate %s mapping in segment %s (first at line %d)",
					kind, seg, mp.Line))
		}
	}
	if !segSeen[seg] {
		segSeen[seg] = true
		m.SegmentOrder = append(m.SegmentOrder, seg)
	}
	m.Mappings = append(m.Mappings, &MappingRule{
		Line: line, Segment: seg, Kind: kind,
		MaterialID: mat.ID, TrackID: track.ID,
	})
	return nil
}

func (m *Manifest) resolveTrack(matID, trackID string, line int, mats map[string]*Material) (*Material, TrackSpec, error) {
	mat, ok := mats[matID]
	if !ok {
		return nil, TrackSpec{}, fail(line, "reference to unknown material %q", matID)
	}
	id, err := strconv.ParseUint(trackID, 10, 32)
	if err != nil || id == 0 {
		return nil, TrackSpec{}, fail(line, "track id must be a positive uint32, got %q", trackID)
	}
	for _, t := range mat.Tracks {
		if t.ID == uint32(id) {
			return mat, t, nil
		}
	}
	return nil, TrackSpec{}, model.At(model.KindInvalidInput, "manifest", op, int64(line),
		fmt.Sprintf("material %s has no track %d", matID, id))
}

func parseInt64(s string, line int, label string) (int64, error) {
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fail(line, "edit %s must be an int64, got %q", label, s)
	}
	return v, nil
}

// crossValidate ensures every EDIT/MAP segment references declared tracks and
// each segment with edits has mappings for all tracks it edits.
func (m *Manifest) crossValidate() error {
	edited := map[string]map[uint32]bool{} // segment -> tracks
	editLine := map[string]int{}           // segment/track -> first EDIT line
	for _, e := range m.Edits {
		set := edited[e.Segment]
		if set == nil {
			set = map[uint32]bool{}
			edited[e.Segment] = set
		}
		set[e.TrackID] = true
		key := fmt.Sprintf("%s/%d", e.Segment, e.TrackID)
		if _, ok := editLine[key]; !ok {
			editLine[key] = e.Line
		}
	}
	mapped := map[string]map[uint32]bool{}
	for _, mp := range m.Mappings {
		set := mapped[mp.Segment]
		if set == nil {
			set = map[uint32]bool{}
			mapped[mp.Segment] = set
		}
		if set[mp.TrackID] {
			return model.At(model.KindConflict, "manifest", op, int64(mp.Line),
				"track mapped twice in segment "+mp.Segment)
		}
		set[mp.TrackID] = true
	}
	for seg, tracks := range edited {
		for tid := range tracks {
			if !mapped[seg][tid] {
				return model.At(model.KindInvalidInput, "manifest", op,
					int64(editLine[fmt.Sprintf("%s/%d", seg, tid)]),
					fmt.Sprintf("segment %s edits track %d but has no MAP for it", seg, tid))
			}
		}
	}
	return nil
}

// FindTrack returns the declared spec for a material/track pair.
func (m *Manifest) FindTrack(materialID string, trackID uint32) (TrackSpec, bool) {
	for _, mat := range m.Materials {
		if mat.ID != materialID {
			continue
		}
		for _, t := range mat.Tracks {
			if t.ID == trackID {
				return t, true
			}
		}
	}
	return TrackSpec{}, false
}
