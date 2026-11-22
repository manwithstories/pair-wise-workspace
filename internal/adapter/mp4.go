package adapter

import (
	"bytes"
	"encoding/binary"
	"io"

	mp4 "github.com/abema/go-mp4"

	"mediaroute/internal/clock"
	"mediaroute/internal/fault"
)

// MP4Parser walks a flat fMP4 box stream and emits events for emsg boxes.
// Truncated boxes and illegal lengths are isolated as parse faults carrying
// the byte offset; other box types are skipped by size.
type MP4Parser struct{}

// NewMP4Parser returns a stateless fMP4/emsg parser.
func NewMP4Parser() *MP4Parser { return &MP4Parser{} }

// MajorVersion implements Parser.
func (p *MP4Parser) MajorVersion() int { return 1 }

// Parse scans one fMP4 byte stream segment.
func (p *MP4Parser) Parse(track string, data []byte, offset int64) ([]Event, error) {
	r := bytes.NewReader(data)
	var events []Event
	for r.Len() > 0 {
		pos := offset + int64(len(data)) - int64(r.Len())
		bi, err := mp4.ReadBoxInfo(r)
		if err != nil {
			return events, fault.Newf(fault.CatParse, "truncated box: %v", err).
				WithTrack(track).WithOffset(pos)
		}
		if bi.Size < bi.HeaderSize || bi.HeaderSize < 8 {
			return events, fault.Newf(fault.CatParse,
				"illegal box length: size=%d header=%d", bi.Size, bi.HeaderSize).
				WithTrack(track).WithOffset(pos)
		}
		if bi.Type.String() == "emsg" {
			ev, err := parseEmsg(r, bi, track, pos)
			if err != nil {
				return events, err
			}
			events = append(events, ev)
			continue
		}
		if _, err := bi.SeekToEnd(r); err != nil {
			return events, fault.Newf(fault.CatParse, "truncated box payload: %v", err).
				WithTrack(track).WithOffset(pos)
		}
	}
	return events, nil
}

// parseEmsg decodes a version-1 emsg box payload:
// fullbox(4) timescale(4) presentation_time(8) event_duration(4) id(4)
// scheme_id_uri\0 value\0 message_data...
func parseEmsg(r *bytes.Reader, bi *mp4.BoxInfo, track string, pos int64) (Event, error) {
	size := int64(bi.Size) - int64(bi.HeaderSize)
	if size < 4+4+8+4+4 {
		return Event{}, fault.Newf(fault.CatParse, "emsg payload too short: %d", size).
			WithTrack(track).WithOffset(pos)
	}
	buf := make([]byte, size)
	if _, err := io.ReadFull(r, buf); err != nil {
		return Event{}, fault.Newf(fault.CatParse, "truncated emsg: %v", err).
			WithTrack(track).WithOffset(pos)
	}
	version := buf[0]
	if version != 1 {
		return Event{}, fault.Newf(fault.CatParse, "unsupported emsg version %d", version).
			WithTrack(track).WithOffset(pos)
	}
	timescale := binary.BigEndian.Uint32(buf[4:8])
	pts := binary.BigEndian.Uint64(buf[8:16])
	dur := binary.BigEndian.Uint32(buf[16:20])
	ts, err := clock.Scale(int64(pts), timescale)
	if err != nil {
		return Event{}, fault.New(fault.CatTiming, err.Error()).
			WithTrack(track).WithRawTime(int64(pts)).WithOffset(pos)
	}
	dns, err := clock.Scale(int64(dur), timescale)
	if err != nil {
		return Event{}, fault.New(fault.CatTiming, err.Error()).
			WithTrack(track).WithRawTime(int64(dur)).WithOffset(pos)
	}
	msg := buf[24:]
	if i := bytes.IndexByte(msg, 0); i >= 0 && i+1 < len(msg) {
		rest := msg[i+1:]
		if j := bytes.IndexByte(rest, 0); j >= 0 {
			msg = rest[j+1:]
		}
	}
	return Event{
		Track:       track,
		Kind:        KindEmsg,
		TimestampNs: ts,
		DurationNs:  dns,
		Payload:     msg,
	}, nil
}

// BuildBox assembles a size+type+payload box; used by the simulator.
func BuildBox(typ string, payload []byte) []byte {
	out := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(out[0:4], uint32(len(out)))
	copy(out[4:8], typ)
	copy(out[8:], payload)
	return out
}

// BuildEmsg assembles a version-1 emsg box; used by the simulator.
func BuildEmsg(timescale uint32, pts uint64, dur, id uint32, scheme, value string, msg []byte) []byte {
	p := make([]byte, 0, 24+len(scheme)+len(value)+len(msg)+2)
	p = append(p, 1, 0, 0, 0) // version 1, flags 0
	p = binary.BigEndian.AppendUint32(p, timescale)
	p = binary.BigEndian.AppendUint64(p, pts)
	p = binary.BigEndian.AppendUint32(p, dur)
	p = binary.BigEndian.AppendUint32(p, id)
	p = append(p, scheme...)
	p = append(p, 0)
	p = append(p, value...)
	p = append(p, 0)
	p = append(p, msg...)
	return BuildBox("emsg", p)
}
