package parse

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"mediarun/internal/clock"
	"mediarun/internal/model"
	"strings"
)

// ParseEmsg decodes an emsg box payload. The layout is parsed by hand
// because every field needs an explicit bounds check; go-mp4's version-1
// string reader consumes to EOF without one.
func ParseEmsg(raw *RawBox) (*EmsgEvent, error) {
	if raw == nil {
		return nil, fmt.Errorf("%w: nil emsg", model.ErrInvalid)
	}
	body := raw.BodyBytes()
	if len(body) < 4 {
		return nil, model.Classify(model.ClassCorrupt,
			fmt.Errorf("%w: emsg shorter than a full box header", model.ErrTruncated))
	}
	version := body[0]
	if version != 0 && version != 1 {
		return nil, model.Classify(model.ClassCorrupt,
			fmt.Errorf("%w: emsg version %d", model.ErrBadLength, version))
	}
	pos := 4
	scheme, next, err := readCString(body, pos)
	if err != nil {
		return nil, err
	}
	pos = next
	value, next, err := readCString(body, pos)
	if err != nil {
		return nil, err
	}
	pos = next
	fixed := 16 // timescale + (presentation) + duration + id
	if version == 1 {
		fixed = 20
	}
	if len(body)-pos < fixed {
		return nil, model.Classify(model.ClassCorrupt,
			fmt.Errorf("%w: emsg fixed fields need %d bytes, have %d", model.ErrTruncated, fixed, len(body)-pos))
	}
	timescale := binary.BigEndian.Uint32(body[pos : pos+4])
	pos += 4
	var presentation uint64
	if version == 0 {
		presentation = uint64(binary.BigEndian.Uint32(body[pos : pos+4]))
		pos += 4
	} else {
		presentation = binary.BigEndian.Uint64(body[pos : pos+8])
		pos += 8
	}
	eventDuration := binary.BigEndian.Uint32(body[pos : pos+4])
	pos += 4
	id := binary.BigEndian.Uint32(body[pos : pos+4])
	pos += 4
	message := body[pos:]
	if timescale == 0 {
		return nil, model.Classify(model.ClassCorrupt,
			fmt.Errorf("%w: emsg timescale is zero", model.ErrBadLength))
	}
	start, err := clock.ScaleToTicks(int64(presentation), timescale)
	if err != nil {
		return nil, model.Classify(model.ClassCorrupt, fmt.Errorf("%w: emsg time: %v", model.ErrBadLength, err))
	}
	dur, err := clock.ScaleToTicks(int64(eventDuration), timescale)
	if err != nil {
		return nil, model.Classify(model.ClassCorrupt, fmt.Errorf("%w: emsg duration: %v", model.ErrBadLength, err))
	}
	return &EmsgEvent{
		ID:        fmt.Sprintf("emsg-%d", id),
		Scheme:    scheme,
		Value:     value,
		Start:     start,
		Duration:  dur,
		Message:   append([]byte(nil), message...),
		Timescale: timescale,
	}, nil
}

// readCString reads a NUL-terminated string and returns the next offset.
func readCString(body []byte, pos int) (string, int, error) {
	for i := pos; i < len(body); i++ {
		if body[i] == 0 {
			return string(body[pos:i]), i + 1, nil
		}
	}
	return "", 0, model.Classify(model.ClassCorrupt,
		fmt.Errorf("%w: emsg string not terminated", model.ErrTruncated))
}

// EncodeEmsg writes a version 1 emsg box payload with absolute time. The
// bytes are laid out by hand so the message data always survives a round
// trip; go-mp4 v1.3.0 drops it on write for version 1.
func EncodeEmsg(ev EmsgEvent) (*RawBox, error) {
	if ev.Timescale == 0 {
		return nil, fmt.Errorf("%w: emsg timescale", model.ErrInvalid)
	}
	if len(ev.Scheme) > MaxSEIPayload || len(ev.Value) > MaxSEIPayload || len(ev.Message) > MaxSEIPayload {
		return nil, fmt.Errorf("%w: emsg field too large", model.ErrLimit)
	}
	if strings.IndexByte(ev.Scheme, 0) >= 0 || strings.IndexByte(ev.Value, 0) >= 0 {
		return nil, fmt.Errorf("%w: emsg string contains NUL", model.ErrInvalid)
	}
	presentation, err := clock.TicksToUnits(ev.Start, ev.Timescale)
	if err != nil {
		return nil, err
	}
	duration, err := clock.TicksToUnits(ev.Duration, ev.Timescale)
	if err != nil {
		return nil, err
	}
	if presentation < 0 || duration < 0 {
		return nil, fmt.Errorf("%w: negative emsg time", model.ErrInvalid)
	}
	var buf bytes.Buffer
	buf.WriteByte(1)           // version 1: absolute presentation time
	buf.Write([]byte{0, 0, 0}) // flags
	buf.WriteString(ev.Scheme)
	buf.WriteByte(0)
	buf.WriteString(ev.Value)
	buf.WriteByte(0)
	writeU32(&buf, ev.Timescale)
	writeU64(&buf, uint64(presentation))
	writeU32(&buf, uint32(duration))
	writeU32(&buf, numericID(ev.ID))
	buf.Write(ev.Message)
	payload := buf.Bytes()
	return &RawBox{
		Type:       [4]byte{'e', 'm', 's', 'g'},
		Size:       uint64(8 + len(payload)),
		HeaderSize: 8,
		Payload:    payload,
	}, nil
}

func writeU32(buf *bytes.Buffer, v uint32) {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	buf.Write(b[:])
}

func writeU64(buf *bytes.Buffer, v uint64) {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], v)
	buf.Write(b[:])
}

func numericID(id string) uint32 {
	var v uint32
	for i := 0; i < len(id); i++ {
		v = v*131 + uint32(id[i])
	}
	return v
}
