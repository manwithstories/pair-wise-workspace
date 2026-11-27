package demux

import (
	"avpipe/internal/clock"
	"avpipe/internal/media"
)

// Unwrapper is the timestamp unwrapper used for one track. It is a type alias
// of clock.Unwrapper so demux callers do not need to import clock directly.
type Unwrapper = clock.Unwrapper

// ValidationError is clock's typed backwards-jump error.
type ValidationError = clock.ValidationError

// NewUnwrapper builds a wrap-aware unwrapper for a track.
func NewUnwrapper(bits int, origin media.Origin) (*Unwrapper, error) {
	cfg := clock.DefaultWrapConfig(bits)
	return clock.NewUnwrapper(cfg, 0, origin)
}
