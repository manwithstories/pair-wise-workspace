// Package pipeline defines runtime safety limits shared by every entry point.
package pipeline

import "mediapack/internal/fault"

// Safety bounds. They are deliberately generous for local media work but
// prevent unbounded memory use and pathological paths.
const (
	// MaxInputBytes caps a single elementary stream input at 256 MiB.
	MaxInputBytes int64 = 256 << 20
	// MaxFragmentBytes caps a single output fragment at 16 MiB.
	MaxFragmentBytes int64 = 16 << 20
	// MaxPathComponents bounds how deep an output path may nest.
	MaxPathComponents = 8
)

// checkSize enforces the single-input size limit before any parsing.
func checkSize(n int64) error {
	if n > MaxInputBytes {
		return fault.At(fault.KindLimit, "pipeline", n,
			"input exceeds configured single-input size limit")
	}
	return nil
}
