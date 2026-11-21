package fixture

import "errors"

// Sample verification failures; kept here so probe can map them onto the
// shared model.CodeCorruptSample vocabulary.
var (
	ErrSampleTooShort = errors.New("fixture: sample shorter than header")
	ErrBadMagic       = errors.New("fixture: bad sample magic")
	ErrIndexMismatch  = errors.New("fixture: sample index mismatch")
	ErrLengthMismatch = errors.New("fixture: sample length mismatch")
	ErrCRCMismatch    = errors.New("fixture: sample CRC mismatch")
)
