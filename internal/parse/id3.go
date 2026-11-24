package parse

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"mediarun/internal/model"
	"strings"
)

// ID3Tag is the subset of ID3v2.3 used for delivery annotations.
type ID3Tag struct {
	ID      string   `json:"id"`
	Owner   string   `json:"owner"`
	Text    string   `json:"text"`
	Private []byte   `json:"private,omitempty"`
	Frames  []string `json:"frames"` // frame identifiers, in order
}

// ParseID3 decodes an ID3v2.3 tag with TXXX and PRIV frames.
func ParseID3(data []byte) (*ID3Tag, error) {
	if len(data) < 10 {
		return nil, model.Classify(model.ClassCorrupt, fmt.Errorf("%w: id3 header", model.ErrTruncated))
	}
	if string(data[0:3]) != "ID3" {
		return nil, model.Classify(model.ClassCorrupt, fmt.Errorf("%w: id3 sync missing", model.ErrBadLength))
	}
	if data[3] != 3 {
		return nil, model.Classify(model.ClassCorrupt,
			fmt.Errorf("%w: id3 version 2.%d", model.ErrBadLength, data[3]))
	}
	size, err := syncSafe(data[6:10])
	if err != nil {
		return nil, err
	}
	if 10+size > len(data) {
		return nil, model.Classify(model.ClassCorrupt,
			fmt.Errorf("%w: id3 tag declares %d bytes, have %d", model.ErrTruncated, size, len(data)-10))
	}
	tag := &ID3Tag{}
	body := data[10 : 10+size]
	pos := 0
	for pos < len(body) {
		if len(body)-pos < 10 {
			return nil, model.Classify(model.ClassCorrupt,
				fmt.Errorf("%w: id3 frame header truncated", model.ErrTruncated))
		}
		id := string(body[pos : pos+4])
		if id == "\x00\x00\x00\x00" {
			break // padding
		}
		frameSize := int(binary.BigEndian.Uint32(body[pos+4 : pos+8]))
		if frameSize < 0 || pos+10+frameSize > len(body) {
			return nil, model.Classify(model.ClassCorrupt,
				fmt.Errorf("%w: id3 frame %q size %d", model.ErrTruncated, id, frameSize))
		}
		frame := body[pos+10 : pos+10+frameSize]
		if len(tag.Frames) > MaxID3Frames {
			return nil, model.Classify(model.ClassCorrupt, fmt.Errorf("%w: id3 frames", model.ErrLimit))
		}
		tag.Frames = append(tag.Frames, id)
		switch id {
		case "TXXX":
			if len(frame) < 2 {
				return nil, model.Classify(model.ClassCorrupt,
					fmt.Errorf("%w: id3 TXXX empty", model.ErrTruncated))
			}
			if frame[0] != 0 {
				return nil, model.Classify(model.ClassCorrupt,
					fmt.Errorf("%w: id3 TXXX encoding %d", model.ErrBadLength, frame[0]))
			}
			parts := bytes.SplitN(frame[1:], []byte{0}, 2)
			if len(parts) != 2 {
				return nil, model.Classify(model.ClassCorrupt,
					fmt.Errorf("%w: id3 TXXX description", model.ErrBadLength))
			}
			if strings.EqualFold(string(parts[0]), "mediarun") {
				tag.Text = string(parts[1])
			} else {
				tag.ID = string(parts[1])
			}
		case "PRIV":
			parts := bytes.SplitN(frame, []byte{0}, 2)
			if len(parts) != 2 {
				return nil, model.Classify(model.ClassCorrupt,
					fmt.Errorf("%w: id3 PRIV owner", model.ErrBadLength))
			}
			tag.Owner = string(parts[0])
			tag.Private = append([]byte(nil), parts[1]...)
		default:
			// Unknown frames are retained by name; the raw tag bytes are
			// preserved separately by the writer.
		}
		pos += 10 + frameSize
	}
	return tag, nil
}

// EncodeID3 writes a canonical ID3v2.3 tag. Frame order is fixed so the
// bytes are reproducible.
func EncodeID3(tag *ID3Tag) ([]byte, error) {
	if tag == nil {
		return nil, fmt.Errorf("%w: nil id3 tag", model.ErrInvalid)
	}
	var frames bytes.Buffer
	writeFrame := func(id string, payload []byte) error {
		if len(payload) > MaxSEIPayload {
			return fmt.Errorf("%w: id3 frame %s", model.ErrLimit, id)
		}
		frames.WriteString(id)
		var size [4]byte
		binary.BigEndian.PutUint32(size[:], uint32(len(payload)))
		frames.Write(size[:])
		frames.Write([]byte{0, 0})
		frames.Write(payload)
		return nil
	}
	if tag.ID != "" {
		payload := append([]byte{0}, []byte("id")...)
		payload = append(payload, 0)
		payload = append(payload, tag.ID...)
		if err := writeFrame("TXXX", payload); err != nil {
			return nil, err
		}
	}
	if tag.Text != "" {
		payload := append([]byte{0}, []byte("mediarun")...)
		payload = append(payload, 0)
		payload = append(payload, tag.Text...)
		if err := writeFrame("TXXX", payload); err != nil {
			return nil, err
		}
	}
	if tag.Owner != "" || len(tag.Private) > 0 {
		payload := append([]byte(tag.Owner), 0)
		payload = append(payload, tag.Private...)
		if err := writeFrame("PRIV", payload); err != nil {
			return nil, err
		}
	}
	tagSize := frames.Len()
	header := []byte{'I', 'D', '3', 3, 0, 0}
	header = append(header, syncSafeBytes(tagSize)...)
	return append(header, frames.Bytes()...), nil
}

func syncSafe(data []byte) (int, error) {
	if len(data) != 4 {
		return 0, model.Classify(model.ClassCorrupt, fmt.Errorf("%w: id3 size field", model.ErrBadLength))
	}
	v := 0
	for _, b := range data {
		if b&0x80 != 0 {
			return 0, model.Classify(model.ClassCorrupt,
				fmt.Errorf("%w: id3 size is not syncsafe", model.ErrBadLength))
		}
		v = v<<7 | int(b)
	}
	return v, nil
}

func syncSafeBytes(v int) []byte {
	return []byte{byte(v >> 21 & 0x7F), byte(v >> 14 & 0x7F), byte(v >> 7 & 0x7F), byte(v & 0x7F)}
}
