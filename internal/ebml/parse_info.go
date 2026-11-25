package ebml

import (
	"bytes"
	"fmt"
	"math"

	"avcheck/internal/media"
)

func parseInfo(c *Cursor, doc *Document, el Element) error {
	body, err := c.ReadBody(el)
	if err != nil {
		return err
	}
	sub := NewCursorAt(bytes.NewReader(body), c.lim, el.BodyOff)
	for {
		child, err := sub.ReadElement()
		if err != nil {
			if isEOF(err) {
				return nil
			}
			return err
		}
		payload, err := sub.ReadBody(child)
		if err != nil {
			return err
		}
		switch child.ID {
		case IDTimecodeScale:
			v, ok := readUint(payload)
			if !ok || v == 0 || v > math.MaxInt32 {
				return &posError{err: ErrBadVint, off: child.HeadOff}
			}
			doc.TimecodeScale = int64(v)
		case IDDuration:
			if len(payload) == 4 || len(payload) == 8 {
				d := readFloat(payload) * float64(doc.TimecodeScale)
				if d > 0 && d < float64(HardMaxDurationNs) {
					doc.DurationNs = int64(d)
				}
			}
		}
	}
}

func parseTracks(c *Cursor, doc *Document, el Element, lim Limits) error {
	body, err := c.ReadBody(el)
	if err != nil {
		return err
	}
	sub := NewCursorAt(bytes.NewReader(body), lim, el.BodyOff)
	for {
		child, err := sub.ReadElement()
		if err != nil {
			if isEOF(err) {
				return nil
			}
			return err
		}
		if child.ID != IDTrackEntry {
			if err := sub.Skip(child.Size); err != nil {
				return err
			}
			continue
		}
		if len(doc.Tracks) >= lim.MaxTracks {
			doc.Faults.Addf(media.LevelAbort, media.CodeBadTrack, 0, child.HeadOff,
				"track count exceeds cap %d", lim.MaxTracks)
			return doc.Faults
		}
		entry, err := sub.ReadBody(child)
		if err != nil {
			return err
		}
		t, ferr := parseTrackEntry(entry, child.BodyOff, lim)
		if ferr != nil {
			doc.Faults.Add(*ferr)
			continue
		}
		doc.Tracks = append(doc.Tracks, t)
	}
}

func parseTrackEntry(body []byte, base int64, lim Limits) (Track, *media.Fault) {
	var t Track
	sub := NewCursorAt(bytes.NewReader(body), lim, base)
	for {
		child, err := sub.ReadElement()
		if err != nil {
			if isEOF(err) {
				break
			}
			f := faultFor(err, base, 0)
			return t, &f
		}
		payload, err := sub.ReadBody(child)
		if err != nil {
			f := faultFor(err, child.HeadOff, 0)
			return t, &f
		}
		switch child.ID {
		case IDTrackNumber:
			t.Number, _ = readUint(payload)
		case IDTrackUID:
			t.UID, _ = readUint(payload)
		case IDTrackType:
			t.Kind = trackKindFromType(payload)
		case IDCodecID:
			t.CodecID = trimZeros(payload)
		case IDTrackName:
			t.Name = trimZeros(payload)
		case IDDefaultDuration:
			if v, ok := readUint(payload); ok {
				t.DefaultDurationNs = int64(v)
			}
		case IDDependsOn:
			if v, ok := readUint(payload); ok {
				t.DependsOn = append(t.DependsOn, v)
			}
		}
	}
	if t.Number == 0 {
		f := media.Fault{Level: media.LevelAbort, Code: media.CodeBadTrack, Offset: base,
			Detail: "track entry missing TrackNumber"}
		return t, &f
	}
	if t.Kind == media.KindUnknown {
		f := media.Fault{Level: media.LevelAbort, Code: media.CodeBadTrack, Track: t.Number, Offset: base,
			Detail: fmt.Sprintf("track %d has unsupported track type", t.Number)}
		return t, &f
	}
	return t, nil
}

func trackKindFromType(b []byte) media.TrackKind {
	v, ok := readUint(b)
	if !ok {
		return media.KindUnknown
	}
	switch v {
	case 1:
		return media.KindVideo
	case 2:
		return media.KindAudio
	case 17:
		return media.KindCue
	}
	return media.KindUnknown
}
