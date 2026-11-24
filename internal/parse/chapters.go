package parse

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"mediarun/internal/clock"
	"mediarun/internal/model"
)

// chpl is the Nero chapter box: FullBox v1, reserved uint32, count uint8,
// then {start uint64 (100 ns), title length uint8, title}.
func parseChpl(raw *RawBox) ([]Chapter, error) {
	if raw == nil {
		return nil, nil
	}
	body := raw.BodyBytes()
	if len(body) < 9 {
		return nil, model.Classify(model.ClassCorrupt,
			fmt.Errorf("%w: chpl shorter than 9 bytes", model.ErrTruncated))
	}
	version := body[0]
	if version != 1 {
		return nil, model.Classify(model.ClassCorrupt,
			fmt.Errorf("%w: chpl version %d", model.ErrBadLength, version))
	}
	count := int(body[8])
	pos := 9
	out := make([]Chapter, 0, count)
	for i := 0; i < count; i++ {
		if pos+9 > len(body) {
			return nil, model.Classify(model.ClassCorrupt,
				fmt.Errorf("%w: chpl entry %d truncated", model.ErrTruncated, i))
		}
		start100ns := binary.BigEndian.Uint64(body[pos : pos+8])
		titleLen := int(body[pos+8])
		pos += 9
		if pos+titleLen > len(body) {
			return nil, model.Classify(model.ClassCorrupt,
				fmt.Errorf("%w: chpl title %d truncated", model.ErrTruncated, i))
		}
		title := string(body[pos : pos+titleLen])
		pos += titleLen
		tick, err := scale100ns(start100ns)
		if err != nil {
			return nil, err
		}
		out = append(out, Chapter{
			ID:    fmt.Sprintf("chapter-%d", i+1),
			Start: tick,
			Title: title,
		})
	}
	if pos != len(body) {
		return nil, model.Classify(model.ClassCorrupt,
			fmt.Errorf("%w: chpl has %d trailing bytes", model.ErrBadLength, len(body)-pos))
	}
	return out, nil
}

func scale100ns(v uint64) (clock.Tick, error) {
	if v > uint64(1)<<62 {
		return 0, fmt.Errorf("%w: chapter start %d", clock.ErrOverflow, v)
	}
	n := int64(v)
	// ticks = n * 9 / 1000, computed without intermediate overflow.
	whole := n / 1000
	rem := n % 1000
	if whole > (1<<62)/9 {
		return 0, fmt.Errorf("%w: chapter start %d", clock.ErrOverflow, v)
	}
	return clock.Tick(whole*9 + rem*9/1000), nil
}

// encodeChpl writes chapters as a chpl box payload (without the header).
func encodeChpl(chapters []Chapter) ([]byte, error) {
	if len(chapters) > 255 {
		return nil, fmt.Errorf("%w: %d chapters", model.ErrLimit, len(chapters))
	}
	var buf bytes.Buffer
	buf.Write([]byte{1, 0, 0, 0}) // version 1, flags 0
	buf.Write([]byte{0, 0, 0, 0}) // reserved
	buf.WriteByte(byte(len(chapters)))
	for _, ch := range chapters {
		if ch.Start < 0 {
			return nil, fmt.Errorf("%w: negative chapter start", model.ErrInvalid)
		}
		units := int64(ch.Start) * 1000 / 9
		if int64(units)*9/1000 != int64(ch.Start) {
			// Keep the exact tick when the conversion is not lossless by
			// rounding up; chapter starts are whole milliseconds in the
			// generated fixtures.
			units = (int64(ch.Start)*1000 + 8) / 9
		}
		var start [8]byte
		binary.BigEndian.PutUint64(start[:], uint64(units))
		buf.Write(start[:])
		if len(ch.Title) > 255 {
			return nil, fmt.Errorf("%w: chapter title too long", model.ErrLimit)
		}
		buf.WriteByte(byte(len(ch.Title)))
		buf.WriteString(ch.Title)
	}
	return buf.Bytes(), nil
}
