package pipeline

import (
	"avcheck/internal/media"
	"avcheck/internal/transport"
)

// trackStream is the simulated wire state for one track.
type trackStream struct {
	track  uint64
	kind   media.TrackKind
	frames []transport.Frame
	result transport.ImpairResult
	buf    *transport.Buffer
	rep    *transport.Repairer
}
