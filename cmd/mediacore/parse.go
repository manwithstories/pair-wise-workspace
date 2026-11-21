package main

import (
	"context"
	"os"

	"mediacore/internal/adapter"
	"mediacore/internal/media"
)

// parseMP4File reparses a delivered output file. Verification always goes
// through the real parser, never through remembered offsets.
func parseMP4File(f *os.File) (*media.Container, error) {
	info, err := f.Stat()
	if err != nil {
		return nil, media.NewError(media.ClassIO, "cannot stat output").WithCause(err)
	}
	src := &fileAt{f: f, size: info.Size(), name: f.Name()}
	return (&adapter.MP4Adapter{}).Parse(context.Background(), src)
}

type fileAt struct {
	f    *os.File
	size int64
	name string
}

func (r *fileAt) ReadAt(p []byte, off int64) (int, error) { return r.f.ReadAt(p, off) }
func (r *fileAt) Size() int64                             { return r.size }
func (r *fileAt) Name() string                            { return r.name }
