package pipeline

import (
	"context"
	"errors"
)

var (
	errInvalidConfig = errors.New("invalid pipeline configuration")
	errSinkFailed    = errors.New("segment sink write failed")
)

// Renderer turns a merged segment into deterministic bytes and their hash.
// Implementations live in internal/render.
type Renderer interface {
	Render(ctx context.Context, seg *Segment) (payload []byte, sha256Hex string, err error)
}
