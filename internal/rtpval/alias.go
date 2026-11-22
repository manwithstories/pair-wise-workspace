// Package rtpval aliases the RTP validation package so the pipeline can refer
// to its types by a distinct name from the upstream pion/rtp library.
package rtpval

import (
	"github.com/pion/rtp"

	rtppkg "mediapack/internal/rtp"
)

// Validator/Event/Issue are re-exported for the pipeline.
type (
	Validator = rtppkg.Validator
	Event     = rtppkg.Event
	Issue     = rtppkg.Issue
)

// NewValidator forwards to internal/rtp.
func NewValidator(trackID uint32) *Validator { return rtppkg.NewValidator(trackID) }

var _ = rtp.Packet{}
