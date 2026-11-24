package testkit

import (
	"encoding/binary"
	"fmt"

	"mediarun/internal/model"
	"mediarun/internal/parse"
)

// entryBox builds a container sample entry with a raw body.
func entryBox(typ string, body []byte) parse.RawBox {
	var t [4]byte
	copy(t[:], typ)
	return parse.RawBox{Type: t, Size: uint64(8 + len(body)), HeaderSize: 8, Payload: body}
}

// avc1Body is the fixed part of a VisualSampleEntry plus an avcC child.
func avc1Body(w, h uint16) []byte {
	body := make([]byte, 78)
	binary.BigEndian.PutUint16(body[0:2], 0)            // reserved
	binary.BigEndian.PutUint16(body[2:4], 1)            // data ref index
	binary.BigEndian.PutUint16(body[24:26], w)          // width
	binary.BigEndian.PutUint16(body[26:28], h)          // height
	binary.BigEndian.PutUint32(body[28:32], 0x00480000) // 72 dpi
	binary.BigEndian.PutUint32(body[32:36], 0x00480000)
	binary.BigEndian.PutUint16(body[40:42], 1)      // frame count
	binary.BigEndian.PutUint16(body[74:76], 0x0018) // depth
	// Minimal avcC so the entry is self-describing.
	avcC := []byte{1, 0x42, 0, 0x1E, 0xFF, 0xE1, 0, 2, 0x67, 0x42, 0x01, 0, 2, 0x68, 0xCE}
	var out []byte
	out = append(out, body...)
	var hdr [8]byte
	binary.BigEndian.PutUint32(hdr[0:4], uint32(8+len(avcC)))
	copy(hdr[4:8], "avcC")
	out = append(out, hdr[:]...)
	out = append(out, avcC...)
	return out
}

// mp4aBody is a minimal AudioSampleEntry.
func mp4aBody(timescale uint32) []byte {
	body := make([]byte, 28)
	binary.BigEndian.PutUint16(body[2:4], 1)    // data ref index
	binary.BigEndian.PutUint16(body[8:10], 2)   // channel count
	binary.BigEndian.PutUint16(body[10:12], 16) // sample size
	binary.BigEndian.PutUint32(body[16:20], timescale<<16)
	return body
}

// encodeChplForTest mirrors the unexported writer in parse; kept here so
// testkit owns all fixture bytes.
func encodeChplForTest(chapters []parse.Chapter) ([]byte, error) {
	if len(chapters) > 255 {
		return nil, fmt.Errorf("%w: chapters", model.ErrLimit)
	}
	var out []byte
	out = append(out, 1, 0, 0, 0, 0, 0, 0, 0, byte(len(chapters)))
	for _, ch := range chapters {
		units := int64(ch.Start) * 1000 / 9
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], uint64(units))
		out = append(out, b[:]...)
		out = append(out, byte(len(ch.Title)))
		out = append(out, ch.Title...)
	}
	return out, nil
}
