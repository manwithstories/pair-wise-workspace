package adapter

import (
	"context"

	"mediacore/internal/media"
)

// Placeholders replaced in the next batch.
type MP4Adapter struct{}
type MXFAdapter struct{}

func (*MP4Adapter) Format() string  { return "mp4" }
func (*MP4Adapter) Version() string { return "1.0.0" }
func (*MP4Adapter) Detect(head []byte, size int64) bool {
	return len(head) >= 8 && string(head[4:8]) == "ftyp"
}
func (*MP4Adapter) Parse(ctx context.Context, src media.Source) (*media.Container, error) {
	return nil, media.NewError(media.ClassUnsupportedCodec, "mp4 adapter not built")
}

func (*MXFAdapter) Format() string  { return "mxf" }
func (*MXFAdapter) Version() string { return "1.0.0" }
func (*MXFAdapter) Detect(head []byte, size int64) bool {
	return len(head) >= 14 && string(head[0:11]) == "\x06\x0e\x2b\x34\x02\x05\x01\x01\x0d\x01\x02"
}
func (*MXFAdapter) Parse(ctx context.Context, src media.Source) (*media.Container, error) {
	return nil, media.NewError(media.ClassUnsupportedCodec, "mxf adapter not built")
}
