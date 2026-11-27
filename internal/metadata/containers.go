package metadata

import (
	"encoding/binary"

	"fastpost/internal/model"
)

// This file parses the four container-level timed-event formats used by
// the drill footage. Every parser owns a small, versioned binary layout
// (the fixtures generate the matching encoders) and reports faults with
// the source track and the exact byte offset.

// ---- subtitles -----------------------------------------------------------

// Subtitle is one decoded cue.
type Subtitle struct {
	Event TimedEvent
	Text  string
}

// ParseSubtitle decodes a length-prefixed cue stream:
//
//	uint32 position, uint32 duration, uint16 textLen, textLen bytes UTF-8
//
// repeated until the blob ends. A record claiming more text than the blob
// contains faults at the first missing byte.
func ParseSubtitle(track string, data []byte, rate RationalLike) ([]Subtitle, error) {
	col := &model.Collector{}
	var out []Subtitle
	i := 0
	for i < len(data) {
		start := i
		if i+10 > len(data) {
			col.Add(model.FaultBoxTruncated, track, int64(len(data)),
				"subtitle header truncated; first missing byte")
			return out, col.Err()
		}
		pos := int64(binary.BigEndian.Uint32(data[i : i+4]))
		dur := int64(binary.BigEndian.Uint32(data[i+4 : i+8]))
		textLen := int(binary.BigEndian.Uint16(data[i+8 : i+10]))
		i += 10
		if i+textLen > len(data) {
			col.Add(model.FaultBoxTruncated, track, int64(len(data)),
				"subtitle text truncated; first missing byte")
			return out, col.Err()
		}
		if dur < 0 {
			col.Add(model.FaultNegativeDuration, track, int64(start), "negative subtitle duration")
			return out, col.Err()
		}
		out = append(out, Subtitle{
			Text: string(data[i : i+textLen]),
			Event: TimedEvent{
				Kind: KindSubtitle, Track: track, Offset: int64(start),
				Position: pos, Duration: dur, RateNum: rate.Num, RateDen: rate.Den,
			},
		})
		i += textLen
	}
	return out, col.Err()
}

// ---- chapters ------------------------------------------------------------

// Chapter is one decoded stage chapter (演练阶段).
type Chapter struct {
	Event TimedEvent
	Title string
}

// ParseChapter decodes the same framing as subtitles but validates that a
// chapter's duration is strictly positive (a zero-length stage is illegal).
func ParseChapter(track string, data []byte, rate RationalLike) ([]Chapter, error) {
	subs, err := ParseSubtitle(track, data, rate)
	col := &model.Collector{}
	if err != nil {
		return nil, err
	}
	var out []Chapter
	for _, s := range subs {
		if s.Event.Duration == 0 {
			col.Add(model.FaultNegativeDuration, track, s.Event.Offset,
				"chapter duration must be positive")
			return out, col.Err()
		}
		out = append(out, Chapter{Title: s.Text, Event: withKind(s.Event, KindChapter)})
	}
	return out, col.Err()
}

func withKind(e TimedEvent, k EventKind) TimedEvent {
	e.Kind = k
	return e
}

// ---- ID3 -----------------------------------------------------------------

// ID3Tag is one decoded ID3v2-style timed tag.
type ID3Tag struct {
	Event  TimedEvent
	TagID  string
	Values map[string]string
}

// ParseID3 decodes a minimal ID3v2.3 frame container:
//
//	"ID3", ver(2 bytes), flags(1), size(4 syncsafe), then frames of
//	fourCC(4), frameSize(4 BE), flags(2), payload.
//
// The first "TXXX"-style frame carries position/duration as two uint32s.
func ParseID3(track string, data []byte, rate RationalLike) ([]ID3Tag, error) {
	col := &model.Collector{}
	if len(data) == 0 {
		return nil, nil
	}
	if len(data) < 10 || string(data[0:3]) != "ID3" {
		col.Add(model.FaultBoxTruncated, track, 0, "missing ID3 header")
		return nil, col.Err()
	}
	total := int(syncSafe(data[6:10]))
	end := 10 + total
	if end > len(data) {
		col.Add(model.FaultBoxTruncated, track, int64(len(data)),
			"ID3 declared size exceeds blob; first missing byte")
		return nil, col.Err()
	}
	i := 10
	var out []ID3Tag
	for i+10 <= end {
		fStart := i
		fourCC := string(data[i : i+4])
		frameSize := int(binary.BigEndian.Uint32(data[i+4 : i+8]))
		i += 10 // header incl. 2 flag bytes
		if i+frameSize > end {
			col.Add(model.FaultBoxTruncated, track, int64(end),
				"ID3 frame exceeds declared tag size")
			return out, col.Err()
		}
		body := data[i : i+frameSize]
		if fourCC == "TXXX" && frameSize >= 8 {
			pos := int64(binary.BigEndian.Uint32(body[0:4]))
			dur := int64(binary.BigEndian.Uint32(body[4:8]))
			out = append(out, ID3Tag{
				TagID:  string(body[8:]),
				Values: map[string]string{"raw": string(body)},
				Event: TimedEvent{
					Kind: KindID3, Track: track, Offset: int64(fStart),
					Position: pos, Duration: dur, RateNum: rate.Num, RateDen: rate.Den,
				},
			})
		}
		i += frameSize
	}
	return out, col.Err()
}

// syncSafe decodes a 4-byte syncsafe integer (7 bits per byte).
func syncSafe(b []byte) uint32 {
	return uint32(b[0])<<21 | uint32(b[1])<<14 | uint32(b[2])<<7 | uint32(b[3])
}

// ---- emsg ----------------------------------------------------------------

// EMSG is one DASH event message box.
type EMSG struct {
	Event      TimedEvent
	SchemeID   string
	Value      string
	Timescale  uint32
	PresentationDelta uint64
}

// ParseEMSG decodes a simplified ISO-BMFF emsg box body (the box header
// "emsg" + size has already been stripped by the fixture, which hands over
// the versioned payload):
//
//	version/flags(4), schemeIdUri (NUL-terminated), value (NUL-terminated),
//	timescale(4), presentationDelta(8), duration(8), id(4).
func ParseEMSG(track string, data []byte, rate RationalLike) ([]EMSG, error) {
	col := &model.Collector{}
	var out []EMSG
	i := 0
	for i < len(data) {
		start := i
		if i+4 > len(data) {
			col.Add(model.FaultBoxTruncated, track, int64(len(data)), "emsg version flags truncated")
			return out, col.Err()
		}
		i += 4
		scheme, n1, ok1 := readCString(data, i)
		if !ok1 {
			col.Add(model.FaultBoxTruncated, track, int64(len(data)), "emsg scheme id unterminated")
			return out, col.Err()
		}
		i = n1
		value, n2, ok2 := readCString(data, i)
		if !ok2 {
			col.Add(model.FaultBoxTruncated, track, int64(len(data)), "emsg value unterminated")
			return out, col.Err()
		}
		i = n2
		if i+20 > len(data) {
			col.Add(model.FaultBoxTruncated, track, int64(len(data)),
				"emsg numeric fields truncated; first missing byte")
			return out, col.Err()
		}
		timescale := binary.BigEndian.Uint32(data[i : i+4])
		pd := binary.BigEndian.Uint64(data[i+4 : i+12])
		dur := int64(binary.BigEndian.Uint64(data[i+12 : i+20]))
		i += 20
		if dur < 0 {
			col.Add(model.FaultNegativeDuration, track, int64(start), "negative emsg duration")
			return out, col.Err()
		}
		out = append(out, EMSG{
			SchemeID: scheme, Value: value, Timescale: timescale, PresentationDelta: pd,
			Event: TimedEvent{
				Kind: KindEMSG, Track: track, Offset: int64(start),
				Position: int64(pd), Duration: dur,
				RateNum: int64(timescale), RateDen: 1,
			},
		})
	}
	return out, col.Err()
}

func readCString(b []byte, start int) (string, int, bool) {
	for j := start; j < len(b); j++ {
		if b[j] == 0 {
			return string(b[start:j]), j + 1, true
		}
	}
	return "", start, false
}
