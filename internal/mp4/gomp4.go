package mp4

import (
	"bytes"
	"fmt"
	"io"

	gomp4 "github.com/abema/go-mp4"
)

// marshalTypedBox uses the pinned go-mp4 marshaller for ISO box payloads and
// prefixes a canonical small-size box header.
func marshalTypedBox(box gomp4.IImmutableBox) ([]byte, error) {
	var payload bytes.Buffer
	n, err := gomp4.Marshal(&payload, box, gomp4.Context{})
	if err != nil {
		return nil, fmt.Errorf("marshal %s: %w", box.GetType(), err)
	}
	info := &gomp4.BoxInfo{
		Type:       box.GetType(),
		Size:       gomp4.SmallHeaderSize + n,
		HeaderSize: gomp4.SmallHeaderSize,
	}
	header := gomp4.EncodeBoxInfo(info)
	out := make([]byte, 0, len(header)+payload.Len())
	out = append(out, header...)
	out = append(out, payload.Bytes()...)
	return out, nil
}

// unmarshalTypedBox uses go-mp4 for bounded payload decoding. The scanner has
// already validated b.Offset and b.Size against the file boundary.
func unmarshalTypedBox(rs io.ReadSeeker, b *Box, dst gomp4.IBox) error {
	if b == nil {
		return fmt.Errorf("nil box for %s", dst.GetType())
	}
	if _, err := rs.Seek(b.Offset+b.HeaderSize, io.SeekStart); err != nil {
		return fmt.Errorf("seek %s payload: %w", b.Type, err)
	}
	payloadSize := b.Size - b.HeaderSize
	if payloadSize < 0 || payloadSize > MaxFileSize {
		return fmt.Errorf("invalid %s payload size %d", b.Type, payloadSize)
	}
	if _, err := gomp4.Unmarshal(rs, uint64(payloadSize), dst, gomp4.Context{}); err != nil {
		return fmt.Errorf("unmarshal %s with go-mp4: %w", b.Type, err)
	}
	return nil
}

func boxTypeString(t gomp4.BoxType) string { return t.String() }
