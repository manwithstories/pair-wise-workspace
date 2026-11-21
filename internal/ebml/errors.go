package ebml

import (
	"errors"
	"fmt"
	"math"

	"archivecheck.local/archivecheck/internal/fault"
)

var (
	// ErrVint marks an invalid variable-length integer.
	ErrVint = errors.New("invalid variable-length integer")
	// ErrTruncated marks input that ended inside a declared structure.
	ErrTruncated = fmt.Errorf("%w", fault.ErrTruncated)
	// ErrOversized marks a declared size that cannot fit its parent.
	ErrOversized = fmt.Errorf("%w", fault.ErrOversized)
	// ErrMalformed marks structurally invalid bytes.
	ErrMalformed = fmt.Errorf("%w", fault.ErrMalformed)
	// ErrBoundary marks a scope that cannot be closed with certainty. It wraps
	// the fault-package sentinel so exit-code classification is shared.
	ErrBoundary = fmt.Errorf("%w: unknown-size element boundary is ambiguous", fault.ErrBoundary)
)

func float64FromBits(b uint64) float64 { return math.Float64frombits(b) }
