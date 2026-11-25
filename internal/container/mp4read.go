package container

import (
	"bytes"
	"fmt"
	"io"
	"os"

	"archivectl/internal/timeline"
)

// ParseFile reads a bounded MP4/fMP4 file and demuxes its archive index.
func ParseFile(path string) (*Asset, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, timeline.Wrap(timeline.CodeInvalidArgument, "stat input", err)
	}
	if !st.Mode().IsRegular() {
		return nil, timeline.New(timeline.CodeInvalidArgument, "read input", "not a regular file")
	}
	if st.Size() > MaxBoxSize {
		return nil, &timeline.Error{Code: timeline.CodeOverflow, Op: "read input", Path: path, Offset: -1,
			Detail: fmt.Sprintf("size %d exceeds bound %d", st.Size(), MaxBoxSize)}
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, timeline.Wrap(timeline.CodeInvalidArgument, "open input", err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, MaxBoxSize+1))
	if err != nil {
		return nil, timeline.Wrap(timeline.CodeCorruptBox, "read input", err)
	}
	if int64(len(data)) > MaxBoxSize {
		return nil, timeline.New(timeline.CodeOverflow, "read input", "file exceeds bounded maximum")
	}
	a, err := ParseBytes(path, data)
	if err != nil {
		return nil, timeline.AtOffset(err, path, offsetOf(err))
	}
	return a, nil
}

func offsetOf(err error) int64 {
	var te *timeline.Error
	if asTimeline(err, &te) && te != nil {
		return te.Offset
	}
	return -1
}

func asTimeline(err error, target **timeline.Error) bool {
	for err != nil {
		if te, ok := err.(*timeline.Error); ok {
			*target = te
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// ParseBytes demuxes one complete MP4 or fMP4 byte sequence.
func ParseBytes(name string, data []byte) (*Asset, error) {
	if len(data) < boxHeaderSize32 {
		return nil, &timeline.Error{Code: timeline.CodeCorruptBox, Op: "parse mp4", Path: name, Offset: 0, Detail: "fewer than 8 bytes"}
	}
	top, err := childHeaders(data, 0, len(data))
	if err != nil {
		return nil, timeline.AtOffset(err, name, offsetOf(err))
	}
	a := &Asset{Path: name, SourceBytes: int64(len(data))}
	for _, h := range top {
		if h.Type == "moof" {
			a.Fragmented = true
		}
	}
	if _, err := inspectWithGoMP4(bytes.NewReader(data)); err != nil {
		return nil, timeline.AtOffset(err, name, offsetOf(err))
	}

	indexHeaders, err := findBoxesByType(data, 0, len(data), "aidx")
	if err != nil {
		return nil, err
	}
	for _, h := range indexHeaders {
		payload := data[h.Offset+h.HeaderSize : h.Offset+int(h.Size)]
		track, err := ParseTrackIndex(payload, int64(h.Offset))
		if err != nil {
			return nil, timeline.AtOffset(err, name, offsetOf(err))
		}
		if err := resolveSampleData(data, &track); err != nil {
			return nil, timeline.AtOffset(err, name, offsetOf(err))
		}
		a.Tracks = append(a.Tracks, track)
	}

	emsgHeaders, err := findBoxesByType(data, 0, len(data), "emsg")
	if err != nil {
		return nil, err
	}
	for _, h := range emsgHeaders {
		payload := data[h.Offset+h.HeaderSize : h.Offset+int(h.Size)]
		ev, err := ParseEMSG(payload, int64(h.Offset))
		if err != nil {
			return nil, timeline.AtOffset(err, name, offsetOf(err))
		}
		ev.RawBox = append([]byte(nil), data[h.Offset:h.Offset+int(h.Size)]...)
		a.Events = append(a.Events, ev)
	}

	all, err := collectReports(data, 0, len(data))
	if err != nil {
		return nil, err
	}
	a.Boxes = all
	a.Tracks = MergeTracks(a.Tracks)
	if err := a.ValidateReferences(); err != nil {
		return nil, timeline.AtOffset(err, name, offsetOf(err))
	}
	return a, nil
}

func resolveSampleData(data []byte, t *Track) error {
	for i := range t.Samples {
		s := &t.Samples[i]
		start := s.FileOffset
		end, overflow := addOverflow(start, int64(s.Size))
		if overflow || start < 0 || end > int64(len(data)) {
			return &timeline.Error{
				Code: timeline.CodeCorruptBox, Op: "resolve sample data", TrackID: t.ID,
				Offset: start, Detail: fmt.Sprintf("sample %d range %d+%d outside %d", i, start, s.Size, len(data)),
			}
		}
		s.Data = append([]byte(nil), data[start:end]...)
		ensureSampleHash(s)
	}
	return nil
}

func addOverflow(a, b int64) (int64, bool) {
	if b > 0 && a > (1<<63-1)-b {
		return 0, true
	}
	if b < 0 && a < (-1<<63)-b {
		return 0, true
	}
	return a + b, false
}

func findBoxesByType(data []byte, start, end int, want string) ([]boxHeader, error) {
	var out []boxHeader
	var walk func(start, end int) error
	walk = func(start, end int) error {
		hs, err := childHeaders(data, start, end)
		if err != nil {
			return err
		}
		for _, h := range hs {
			if h.Type == want {
				out = append(out, h)
			}
			if isContainerType(h.Type) {
				if err := walk(h.Offset+h.HeaderSize, h.Offset+int(h.Size)); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(start, end); err != nil {
		return nil, err
	}
	return out, nil
}

func collectReports(data []byte, start, end int) ([]BoxReport, error) {
	var out []BoxReport
	var walk func(start, end int) error
	walk = func(start, end int) error {
		hs, err := childHeaders(data, start, end)
		if err != nil {
			return err
		}
		for _, h := range hs {
			out = append(out, BoxReport{Type: h.Type, Offset: int64(h.Offset), Size: h.Size, HeaderSize: h.HeaderSize})
			if isContainerType(h.Type) {
				if err := walk(h.Offset+h.HeaderSize, h.Offset+int(h.Size)); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(start, end); err != nil {
		return nil, err
	}
	return out, nil
}

// MergeAssets combines independently parsed init/fragment byte sequences.
func MergeAssets(parts ...*Asset) *Asset {
	merged := &Asset{Fragmented: true}
	tracks := make([]Track, 0)
	for _, p := range parts {
		if p == nil {
			continue
		}
		merged.Events = append(merged.Events, p.Events...)
		merged.Boxes = append(merged.Boxes, p.Boxes...)
		merged.GapRanges += p.GapRanges
		merged.GapPackets += p.GapPackets
		if p.Partial {
			merged.Partial = true
		}
		tracks = append(tracks, p.Tracks...)
	}
	merged.Tracks = MergeTracks(tracks)
	return merged
}

// MergeTracks concatenates samples from repeated fragment indexes by ID.
func MergeTracks(tracks []Track) []Track {
	byID := make(map[uint32]*Track)
	order := make([]uint32, 0)
	for _, t := range tracks {
		cp := t
		if cur, ok := byID[t.ID]; ok {
			if cur.Timescale == 0 {
				cur.Timescale = t.Timescale
			}
			if cur.Kind == "" || cur.Kind == KindMetadata {
				cur.Kind = t.Kind
			}
			cur.Samples = append(cur.Samples, t.Samples...)
			cur.References = append(cur.References, t.References...)
			continue
		}
		byID[t.ID] = &cp
		order = append(order, t.ID)
	}
	out := make([]Track, 0, len(order))
	for _, id := range order {
		t := byID[id]
		for i := range t.Samples {
			t.Samples[i].Index = i
		}
		out = append(out, *t)
	}
	return out
}
