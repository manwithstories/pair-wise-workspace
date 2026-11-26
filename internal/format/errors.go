package format

import "errors"

var (
	errTruncated    = errors.New("field extends beyond container")
	errBadBoxLength = errors.New("box length is outside its enclosing range")
	errBadBERLength = errors.New("invalid or non-canonical KLV BER length")
	errUnsupported  = errors.New("unsupported metadata subset")
	errTooLarge     = errors.New("input exceeds bounded media lab limit")
)
