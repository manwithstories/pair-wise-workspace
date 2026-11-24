package parse

import (
	"encoding/binary"
	"fmt"
	"io"

	"mediarun/internal/model"
)

// Scan reads the whole container from r and returns its top-level boxes.
// Every malformed length, truncation or nesting violation is an explicit
// error; the scanner never guesses a size.
func Scan(r io.Reader) ([]RawBox, error) {
	data, err := io.ReadAll(io.LimitReader(r, MaxFileBytes+1))
	if err != nil {
		return nil, model.Classify(model.ClassCorrupt, fmt.Errorf("parse: read: %w", err))
	}
	if len(data) > MaxFileBytes {
		return nil, model.Classify(model.ClassCorrupt,
			fmt.Errorf("%w: file exceeds %d bytes", model.ErrLimit, MaxFileBytes))
	}
	count := 0
	boxes, err := scanRange(data, 0, int64(len(data)), "", 0, &count)
	if err != nil {
		return nil, err
	}
	if len(boxes) == 0 {
		return nil, model.Classify(model.ClassCorrupt, fmt.Errorf("%w: no boxes", model.ErrTruncated))
	}
	return boxes, nil
}

func scanRange(data []byte, start, end int64, parent string, depth int, count *int) ([]RawBox, error) {
	if depth > MaxBoxDepth {
		return nil, model.Classify(model.ClassCorrupt,
			fmt.Errorf("%w: box depth exceeds %d", model.ErrLimit, MaxBoxDepth))
	}
	var out []RawBox
	pos := start
	for pos < end {
		remaining := end - pos
		if remaining < 8 {
			return nil, model.Classify(model.ClassCorrupt,
				fmt.Errorf("%w: %d trailing bytes in %s", model.ErrTruncated, remaining, parent))
		}
		*count++
		if *count > MaxBoxes {
			return nil, model.Classify(model.ClassCorrupt,
				fmt.Errorf("%w: more than %d boxes", model.ErrLimit, MaxBoxes))
		}
		hdr := data[pos : pos+8]
		size := uint64(binary.BigEndian.Uint32(hdr[0:4]))
		var typ [4]byte
		copy(typ[:], hdr[4:8])
		header := uint64(8)
		switch {
		case size == 1:
			if remaining < 16 {
				return nil, model.Classify(model.ClassCorrupt,
					fmt.Errorf("%w: large box header in %s", model.ErrTruncated, parent))
			}
			size = binary.BigEndian.Uint64(data[pos+8 : pos+16])
			header = 16
		case size == 0:
			// Extends to the end of the enclosing container.
			size = uint64(remaining)
		case size < 8:
			return nil, model.Classify(model.ClassCorrupt,
				fmt.Errorf("%w: box %q declares size %d", model.ErrBadLength, typ, size))
		}
		if size > uint64(remaining) {
			return nil, model.Classify(model.ClassCorrupt,
				fmt.Errorf("%w: box %q size %d exceeds %d available bytes",
					model.ErrTruncated, typ, size, remaining))
		}
		if size < header {
			return nil, model.Classify(model.ClassCorrupt,
				fmt.Errorf("%w: box %q size %d smaller than header %d",
					model.ErrBadLength, typ, size, header))
		}
		box := RawBox{
			Type:       typ,
			Offset:     pos,
			Size:       size,
			HeaderSize: header,
			Parent:     parent,
		}
		payload := data[pos+int64(header) : pos+int64(size)]
		if uint64(len(payload)) > MaxBoxPayload {
			return nil, model.Classify(model.ClassCorrupt,
				fmt.Errorf("%w: box %q payload %d", model.ErrLimit, typ, len(payload)))
		}
		name := string(typ[:])
		if prefix, ok := containerPrefix[name]; ok {
			if len(payload) < prefix {
				return nil, model.Classify(model.ClassCorrupt,
					fmt.Errorf("%w: box %q payload %d shorter than prefix %d",
						model.ErrTruncated, typ, len(payload), prefix))
			}
			box.Payload = append([]byte(nil), payload[:prefix]...)
			childStart := pos + int64(header) + int64(prefix)
			children, err := scanRange(data, childStart, pos+int64(size), name, depth+1, count)
			if err != nil {
				return nil, err
			}
			box.Children = children
		} else {
			// Known and unknown leaves both keep their payload verbatim;
			// unknown boxes are never interpreted.
			box.Payload = append([]byte(nil), payload...)
		}
		out = append(out, box)
		pos += int64(size)
	}
	return out, nil
}
