package index

import "errors"

// Index durability errors.
var (
	ErrEmptyDir = errors.New("index: empty store directory")
	ErrMkdir    = errors.New("index: cannot create store directory")
	ErrWrite    = errors.New("index: write failed")
	ErrRename   = errors.New("index: atomic rename failed")
	ErrEncode   = errors.New("index: encode failed")
	ErrDecode   = errors.New("index: decode failed")
	ErrRead     = errors.New("index: read failed")
	ErrNotFound = errors.New("index: file not found")
	ErrChecksum = errors.New("index: checksum mismatch")
	ErrDigest   = errors.New("index: input digest mismatch, resume refused")
	ErrCursor   = errors.New("index: checkpoint cursor invalid")
)
