package manifest

import (
	"io"

	"mediad/internal/model"
)

// CursorSource adapts an ingest cursor to a FrameSource. It pulls one frame at
// a time, so alignment never holds more than the current frame.
type CursorSource struct {
	NextFn func() (model.Frame, error)
}

func (c *CursorSource) Next() (FrameRef, error) {
	fr, err := c.NextFn()
	if err != nil {
		if err == io.EOF {
			return FrameRef{}, io.EOF
		}
		return FrameRef{}, err
	}
	return FrameRef{
		FrameNo:      fr.Meta.FrameNo,
		PTS:          fr.PTS,
		CaptureUnits: fr.Meta.CaptureUnits,
		Usable:       fr.Status.Usable(),
	}, nil
}
