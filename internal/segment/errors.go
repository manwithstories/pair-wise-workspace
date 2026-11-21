package segment

import "errors"

// Segment-package planning errors.
var (
	ErrInvalidPlanConfig = errors.New("segment: invalid planner configuration")
	ErrNoRepresentations = errors.New("segment: at least one representation required")
	ErrEmptyTimeline     = errors.New("segment: track has no samples on timeline")
	ErrAlignmentFailed   = errors.New("segment: representation alignment failed beyond bounded fallback")
)
