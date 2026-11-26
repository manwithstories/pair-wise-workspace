package pipeline

import (
	"context"

	"mediad/internal/fault"
	"mediad/internal/manifest"
	"mediad/internal/timeline"
)

// runVideo is a compilable stub; the streaming implementation lands in a
// follow-up patch.
func runVideo(ctx context.Context, clock timeline.Clock, t *manifest.Track,
	opts Options, res *Result, faults *fault.List) error {
	return nil
}
