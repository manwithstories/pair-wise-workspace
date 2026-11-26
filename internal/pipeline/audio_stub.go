package pipeline

import (
	"mediad/internal/fault"
	"mediad/internal/manifest"
)

// runAudio is a compilable stub; the real implementation lands together
// with the frame verification helpers in a follow-up patch.
func runAudio(t *manifest.Track, opts Options, res *Result, faults *fault.List) error {
	return nil
}
