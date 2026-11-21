package adapter

import (
	"context"

	"mediacore/internal/media"
)

// MXFAdapter will parse the declared, code-generated MXF subset. It is kept
// as a distinct registered version so selecting it is explicit.
type MXFAdapter struct{}

func (*MXFAdapter) Format() string  { return "mxf" }
func (*MXFAdapter) Version() string { return "1.0.0" }
func (*MXFAdapter) Detect(head []byte, size int64) bool {
	return len(head) >= 14 && string(head[0:11]) == "\x06\x0e\x2b\x34\x02\x05\x01\x01\x0d\x01\x02"
}
func (*MXFAdapter) Parse(ctx context.Context, src media.Source) (*media.Container, error) {
	return nil, media.NewError(media.ClassUnsupportedCodec, "mxf adapter is not built yet")
}
