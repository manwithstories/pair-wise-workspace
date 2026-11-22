// Package meta parses MP4 emsg boxes and aligns timed metadata events
// (subtitles, chapters, ID3, SEI) onto the shared media timeline.
package meta

import (
	"bytes"
	"fmt"

	mp4 "github.com/abema/go-mp4"
)

// EmsgEvent is one DASH event message carried in an MP4 emsg box.
type EmsgEvent struct {
	SchemeID         string
	Value            string
	Timescale        uint32
	PresentationTime uint64 // in Timescale units
	Duration         uint32
	ID               uint32
	Data             []byte
}

// BuildSegment serialises events into an MP4 segment (styp + emsg boxes)
// using version-1 emsg with absolute presentation times.
func BuildSegment(events []EmsgEvent) ([]byte, error) {
	var buf bytes.Buffer
	styp := &mp4.Styp{
		MajorBrand:   [4]byte{'m', 's', 'd', 'h'},
		MinorVersion: 0,
		CompatibleBrands: []mp4.CompatibleBrandElem{
			{CompatibleBrand: [4]byte{'m', 's', 'd', 'h'}},
		},
	}
	if _, err := mp4.Marshal(&buf, styp, mp4.Context{}); err != nil {
		return nil, fmt.Errorf("meta: marshal styp: %w", err)
	}
	for _, ev := range events {
		if ev.Timescale == 0 {
			return nil, fmt.Errorf("meta: emsg id %d: zero timescale", ev.ID)
		}
		box := &mp4.Emsg{
			SchemeIdUri:      ev.SchemeID,
			Value:            ev.Value,
			Timescale:        ev.Timescale,
			PresentationTime: ev.PresentationTime,
			EventDuration:    ev.Duration,
			Id:               ev.ID,
			MessageData:      ev.Data,
		}
		box.SetVersion(1)
		if _, err := mp4.Marshal(&buf, box, mp4.Context{}); err != nil {
			return nil, fmt.Errorf("meta: marshal emsg id %d: %w", ev.ID, err)
		}
	}
	return buf.Bytes(), nil
}

// ParseSegment decodes the emsg events of a segment built by
// BuildSegment. Truncated or malformed input yields a located error.
func ParseSegment(data []byte) ([]EmsgEvent, error) {
	var events []EmsgEvent
	_, err := mp4.ReadBoxStructure(bytes.NewReader(data), func(h *mp4.ReadHandle) (interface{}, error) {
		if h.BoxInfo.Type != mp4.BoxTypeEmsg() {
			return nil, nil
		}
		payload, _, err := h.ReadPayload()
		if err != nil {
			return nil, fmt.Errorf("meta: read emsg at offset %d: %w", h.BoxInfo.Offset, err)
		}
		emsg, ok := payload.(*mp4.Emsg)
		if !ok {
			return nil, fmt.Errorf("meta: unexpected payload %T at offset %d", payload, h.BoxInfo.Offset)
		}
		events = append(events, EmsgEvent{
			SchemeID:         emsg.SchemeIdUri,
			Value:            emsg.Value,
			Timescale:        emsg.Timescale,
			PresentationTime: emsg.PresentationTime,
			Duration:         emsg.EventDuration,
			ID:               emsg.Id,
			Data:             append([]byte(nil), emsg.MessageData...),
		})
		return nil, nil
	})
	if err != nil {
		return nil, fmt.Errorf("meta: parse segment: %w", err)
	}
	return events, nil
}
