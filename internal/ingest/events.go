package ingest

import (
	"bytes"
	"encoding/binary"

	mp4 "github.com/abema/go-mp4"

	"mediacq/internal/clock"
)

// Event is a normalized timed-metadata occurrence on the unified timeline.
type Event struct {
	Family   string // subtitle|chapter|id3|emsg|sei
	ID       uint32
	Time     clock.Time
	Duration clock.Time
	// Origin keeps the source timebase and raw tick for audit/error reports.
	Origin   clock.Rate
	RawTicks int64
	Offset   int64
}

// Family names, exported for report/packager consumers.
const (
	FamilySubtitle = "subtitle"
	FamilyChapter  = "chapter"
	FamilyID3      = "id3"
	FamilyEmsg     = "emsg"
	FamilySEI      = "sei"
)

// udtaTag maps our custom fourcc tags to normalized families.
var udtaTag = map[string]string{
	"mqt ": FamilySubtitle,
	"mqch": FamilyChapter,
	"id3 ": FamilyID3,
	"msei": FamilySEI,
}

// ParseEvents walks top-level emsg boxes and udta child tags, converting each
// timestamp from its originating clock onto the unified nanosecond axis.
// Custom udta fourccs are walked manually: go-mp4's registry does not know
// them and ExtractBoxes would silently skip them.
func ParseEvents(data []byte) ([]Event, error) {
	r := bytes.NewReader(data)
	bis, err := mp4.ExtractBoxes(r, nil, []mp4.BoxPath{
		{mp4.BoxTypeEmsg()},
		{mp4.BoxTypeMoov(), mp4.BoxTypeUdta()},
	})
	if err != nil {
		return nil, &BoxError{Box: "udta", Detail: err.Error()}
	}
	events := make([]Event, 0)
	for _, bi := range bis {
		switch {
		case bi.Type == mp4.BoxTypeEmsg():
			ev, err := parseEmsg(r, bi)
			if err != nil {
				return nil, err
			}
			events = append(events, ev)
		case bi.Type == mp4.BoxTypeUdta():
			evs, err := parseUdtaChildren(data, bi)
			if err != nil {
				return nil, err
			}
			events = append(events, evs...)
		}
	}
	return events, nil
}

func parseEmsg(r *bytes.Reader, bi *mp4.BoxInfo) (Event, error) {
	if _, err := bi.SeekToPayload(r); err != nil {
		return Event{}, &BoxError{Offset: int64(bi.Offset), Box: "emsg", Detail: err.Error()}
	}
	payload := make([]byte, bi.Size-bi.HeaderSize)
	if _, err := r.Read(payload); err != nil {
		return Event{}, &BoxError{Offset: int64(bi.Offset), Box: "emsg", Detail: err.Error()}
	}
	// version(1) flags(3) scheme\0 value\0 timescale(4) delta(4) dur(4) id(4)
	if len(payload) < 4 {
		return Event{}, &BoxError{Offset: int64(bi.Offset), Box: "emsg", Detail: "truncated header"}
	}
	pos := 4 // skip version+flags
	scheme, n := readCString(payload[pos:])
	pos += n
	_, n = readCString(payload[pos:])
	pos += n
	_ = scheme
	if pos+16 > len(payload) {
		return Event{}, &BoxError{Offset: int64(bi.Offset), Box: "emsg", Detail: "fields past payload"}
	}
	timescale := binary.BigEndian.Uint32(payload[pos:])
	raw := int64(binary.BigEndian.Uint32(payload[pos+4:]))
	durTicks := int64(binary.BigEndian.Uint32(payload[pos+8:]))
	id := binary.BigEndian.Uint32(payload[pos+12:])
	rate, err := clock.NewRate(int64(timescale), 1)
	if err != nil {
		return Event{}, &clock.Error{Op: "emsg", TrackID: 0, RawTS: raw,
			Offset: int64(bi.Offset), Err: err}
	}
	return makeEvent(FamilyEmsg, id, rate, raw, durTicks, int64(bi.Offset))
}

// parseUdtaChildren walks the raw udta payload so unregistered fourccs are
// never silently dropped. Each child body is 20 bytes:
// num(4) den(4) raw(4) id(4) payloadLen(4).
func parseUdtaChildren(data []byte, udta *mp4.BoxInfo) ([]Event, error) {
	payStart := udta.Offset + udta.HeaderSize
	payEnd := udta.Offset + udta.Size
	out := make([]Event, 0)
	pos := payStart
	for pos+8 <= payEnd {
		size := uint64(binary.BigEndian.Uint32(data[pos:]))
		tag := string(data[pos+4 : pos+8])
		if size < 8 || pos+size > payEnd {
			return nil, &BoxError{Offset: int64(pos), Box: tag,
				Detail: "udta child size exceeds parent bounds"}
		}
		family, ok := udtaTag[tag]
		if !ok {
			pos += size
			continue
		}
		body := data[pos+8 : pos+size]
		if len(body) < 20 {
			return nil, &BoxError{Offset: int64(pos), Box: tag, Detail: "metadata body truncated"}
		}
		num := int64(binary.BigEndian.Uint32(body[0:]))
		den := int64(binary.BigEndian.Uint32(body[4:]))
		raw := int64(binary.BigEndian.Uint32(body[8:]))
		id := binary.BigEndian.Uint32(body[12:])
		rate, err := clock.NewRate(num, den)
		if err != nil {
			return nil, &clock.Error{Op: family, TrackID: 0, RawTS: raw,
				Offset: int64(pos), Err: err}
		}
		ev, err := makeEvent(family, id, rate, raw, 0, int64(pos))
		if err != nil {
			return nil, err
		}
		out = append(out, ev)
		pos += size
	}
	return out, nil
}

func makeEvent(family string, id uint32, rate clock.Rate, raw, durTicks, offset int64) (Event, error) {
	t, lerr := rate.ToTimeLocated(raw, 0, offset)
	if lerr != nil {
		lerr.Op = family
		return Event{}, lerr
	}
	var dur clock.Time
	if durTicks > 0 {
		d, err := rate.Duration(durTicks)
		if err != nil {
			return Event{}, &clock.Error{Op: family, RawTS: raw, Offset: offset, Err: err}
		}
		dur = clock.Time(d)
	}
	return Event{
		Family: family, ID: id, Time: t, Duration: dur,
		Origin: rate, RawTicks: raw, Offset: offset,
	}, nil
}

func readCString(b []byte) (string, int) {
	for i, c := range b {
		if c == 0 {
			return string(b[:i]), i + 1
		}
	}
	return string(b), len(b)
}
