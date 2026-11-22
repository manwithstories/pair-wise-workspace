// Package meta parses MP4 emsg boxes and aligns timed metadata events
// (subtitles, chapters, ID3, SEI) onto the shared media timeline.
package meta

import (
	"bytes"
	"encoding/binary"
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
// using version-1 emsg with absolute presentation times. mp4.Marshal
// emits box payloads only, so each box is framed with its size+type
// header here.
func BuildSegment(events []EmsgEvent) ([]byte, error) {
	var buf bytes.Buffer
	styp := &mp4.Styp{
		MajorBrand:   [4]byte{'m', 's', 'd', 'h'},
		MinorVersion: 0,
		CompatibleBrands: []mp4.CompatibleBrandElem{
			{CompatibleBrand: [4]byte{'m', 's', 'd', 'h'}},
		},
	}
	if err := marshalBox(&buf, mp4.BoxTypeStyp(), styp); err != nil {
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
		if err := marshalBox(&buf, mp4.BoxTypeEmsg(), box); err != nil {
			return nil, fmt.Errorf("meta: marshal emsg id %d: %w", ev.ID, err)
		}
	}
	return buf.Bytes(), nil
}

func marshalBox(buf *bytes.Buffer, typ mp4.BoxType, box mp4.IImmutableBox) error {
	var payload bytes.Buffer
	if _, err := mp4.Marshal(&payload, box, mp4.Context{}); err != nil {
		return err
	}
	head := make([]byte, 8)
	binary.BigEndian.PutUint32(head, uint32(payload.Len()+8))
	copy(head[4:], typ[:])
	buf.Write(head)
	buf.Write(payload.Bytes())
	return nil
}

// ParseSegment decodes the emsg events of a segment built by
// BuildSegment. Box framing is validated first (go-mp4 is lenient at
// the root level), so truncated or size-corrupted input yields a
// located error.
func ParseSegment(data []byte) ([]EmsgEvent, error) {
	if err := validateBoxes(data); err != nil {
		return nil, err
	}
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

// validateBoxes walks the top-level box structure and verifies every
// box is well-framed: a complete header, a sane size, and a payload
// that fits inside the input.
func validateBoxes(data []byte) error {
	for off := 0; off < len(data); {
		if len(data)-off < 8 {
			return fmt.Errorf("meta: truncated box header at offset %d", off)
		}
		size := uint64(binary.BigEndian.Uint32(data[off : off+4]))
		hdr := uint64(8)
		if size == 1 {
			if len(data)-off < 16 {
				return fmt.Errorf("meta: truncated largesize header at offset %d", off)
			}
			size = binary.BigEndian.Uint64(data[off+8 : off+16])
			hdr = 16
		} else if size == 0 {
			size = uint64(len(data) - off) // box extends to end of input
		}
		if size < hdr {
			return fmt.Errorf("meta: box at offset %d has invalid size %d", off, size)
		}
		if size > uint64(len(data)-off) {
			return fmt.Errorf("meta: box at offset %d size %d exceeds remaining %d bytes",
				off, size, len(data)-off)
		}
		off += int(size)
	}
	return nil
}
