// Package ingest turns synthetic MP4 containers and RTP packet streams into
// validated sample flows and normalized timed events on the unified timeline.
// All failures are returned as located errors (track id, box offset, raw
// timestamp); sample payloads are never copied into error strings.
package ingest

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	mp4 "github.com/abema/go-mp4"

	"mediacq/internal/clock"
)

// ErrCorrupt is the sentinel for malformed container structure.
var ErrCorrupt = errors.New("ingest: corrupt mp4 structure")

// BoxError locates a structural failure inside the container.
type BoxError struct {
	Offset int64
	Box    string
	Detail string
}

func (e *BoxError) Error() string {
	return fmt.Sprintf("%s: box=%s offset=%d: %s", ErrCorrupt.Error(), e.Box, e.Offset, e.Detail)
}
func (e *BoxError) Unwrap() error { return ErrCorrupt }

// Sample is one parsed media sample on the unified timeline.
type Sample struct {
	TrackID    uint32
	Index      int
	Start      clock.Time
	Duration   clock.Time // length as a time delta
	Size       uint32
	Keyframe   bool
	Payload    []byte
	FileOffset int64
}

// Track is a fully validated media track.
type Track struct {
	ID        uint32
	Kind      string
	Timescale uint32
	Rate      clock.Rate
	Samples   []Sample
	// KeyframeIndices lists 0-based random-access sample numbers.
	KeyframeIndices []int
}

// Container is the validated parse result.
type Container struct {
	Tracks []*Track
	// RawBoxes records every traversed box offset for hierarchy diagnostics.
	RawBoxes []BoxRecord
}

// BoxRecord is one box observed during the hierarchy walk.
type BoxRecord struct {
	Type   string
	Offset int64
	Size   uint64
	Depth  int
}

// Parse validates box hierarchy and sample tables, then materializes sample
// timelines. A truncated box, illegal nesting or missing keyframe fails the
// parse before any sample is returned — no fabricated samples are produced.
func Parse(data []byte) (*Container, error) {
	r := bytes.NewReader(data)
	c := &Container{}
	if err := validateHierarchy(r, c); err != nil {
		return nil, err
	}
	info, err := mp4.Probe(bytes.NewReader(data))
	if err != nil {
		return nil, &BoxError{Offset: 0, Box: "moov", Detail: err.Error()}
	}
	if len(info.Tracks) == 0 {
		return nil, &BoxError{Offset: 0, Box: "moov", Detail: "no tracks present"}
	}
	for i, tr := range info.Tracks {
		t, err := buildTrack(data, i, tr)
		if err != nil {
			return nil, err
		}
		c.Tracks = append(c.Tracks, t)
	}
	return c, nil
}

// validateHierarchy walks every top-level and nested box, checking declared
// sizes against parent bounds and the EOF, and enforcing required nesting.
func validateHierarchy(r io.ReadSeeker, c *Container) error {
	r.Seek(0, io.SeekStart)
	end := rSeekEnd(r)
	return walkBoxes(r, 0, end, 0, c, map[string]bool{
		"moov": false, "ftyp": false,
	})
}

func rSeekEnd(r io.Seeker) int64 {
	end, _ := r.Seek(0, io.SeekEnd)
	return end
}

// containerBoxes are boxes that legally contain child boxes.
var containerBoxes = map[string]bool{
	"moov": true, "trak": true, "mdia": true, "minf": true,
	"stbl": true, "udta": true, "edts": true,
}

func walkBoxes(r io.ReadSeeker, start, end int64, depth int, c *Container, require map[string]bool) error {
	pos := start
	for pos < end {
		if _, err := r.Seek(pos, io.SeekStart); err != nil {
			return err
		}
		bi, err := mp4.ReadBoxInfo(r)
		if err != nil {
			return &BoxError{Offset: pos, Box: "?", Detail: "unreadable box header: " + err.Error()}
		}
		boxEnd := int64(bi.Offset + bi.Size)
		if bi.Size < 8 {
			return &BoxError{Offset: pos, Box: bi.Type.String(), Detail: "box smaller than 8-byte header"}
		}
		if boxEnd > end {
			return &BoxError{Offset: pos, Box: bi.Type.String(),
				Detail: fmt.Sprintf("declared end %d exceeds parent/EOF %d", boxEnd, end)}
		}
		c.RawBoxes = append(c.RawBoxes, BoxRecord{
			Type: bi.Type.String(), Offset: int64(bi.Offset), Size: bi.Size, Depth: depth,
		})
		if containerBoxes[bi.Type.String()] {
			payloadStart, err := bi.SeekToPayload(r)
			if err != nil {
				return &BoxError{Offset: pos, Box: bi.Type.String(), Detail: err.Error()}
			}
			if err := walkBoxes(r, payloadStart, boxEnd, depth+1, c, nil); err != nil {
				return err
			}
		}
		pos = boxEnd
	}
	if pos != end {
		return &BoxError{Offset: pos, Box: "*", Detail: "stray bytes outside box boundaries"}
	}
	return nil
}
