package ebml

import (
	"bytes"
	"fmt"

	"avcheck/internal/media"
)

// parseCluster reads a cluster body. An unknown-size cluster ends when the
// next sibling ID (cluster/info/tracks/cues) or the segment end is reached.
func parseCluster(c *Cursor, doc *Document, el Element, lim Limits, segEnd int64) error {
	var timecode int64
	end := int64(-1)
	if !el.Unknown {
		end = el.BodyOff + el.Size
	}
	for {
		if end >= 0 && c.Offset() >= end {
			return nil
		}
		id, err := c.PeekID()
		if err != nil {
			if isEOF(err) {
				if end >= 0 {
					return &posError{err: ErrTruncated, off: c.Offset()}
				}
				return nil
			}
			return err
		}
		if el.Unknown && isSiblingID(id.Value) {
			return nil
		}
		if segEnd >= 0 && c.Offset() >= segEnd {
			return nil
		}
		child, err := c.ReadElement()
		if err != nil {
			return err
		}
		if err := checkExtent(child, end); err != nil {
			return err
		}
		switch child.ID {
		case IDTimecode:
			payload, err := c.ReadBody(child)
			if err != nil {
				return err
			}
			v, ok := readUint(payload)
			if !ok {
				return &posError{err: ErrBadVint, off: child.HeadOff}
			}
			timecode = int64(v)
		case IDSimpleBlock:
			if err := parseBlock(c, doc, child, timecode, 0, lim); err != nil {
				return err
			}
		case IDBlockGroup:
			if err := parseBlockGroup(c, doc, child, timecode, lim); err != nil {
				return err
			}
		default:
			if child.Unknown {
				return fmt.Errorf("%w: unknown-size child 0x%X in cluster at offset %d", ErrTooLarge, child.ID, child.HeadOff)
			}
			if err := c.Skip(child.Size); err != nil {
				return err
			}
		}
	}
}

func isSiblingID(id uint64) bool {
	switch id {
	case IDCluster, IDTracks, IDInfo, IDCues, IDSegment:
		return true
	}
	return false
}

func parseBlockGroup(c *Cursor, doc *Document, el Element, clusterTc int64, lim Limits) error {
	body, err := c.ReadBody(el)
	if err != nil {
		return err
	}
	sub := NewCursorAt(bytes.NewReader(body), lim, el.BodyOff)
	var duration int64
	var blockBody []byte
	var blockOff int64
	found := false
	for {
		child, err := sub.ReadElement()
		if err != nil {
			if isEOF(err) {
				break
			}
			return err
		}
		payload, err := sub.ReadBody(child)
		if err != nil {
			return err
		}
		switch child.ID {
		case IDBlockDuration:
			if v, ok := readUint(payload); ok {
				duration = int64(v)
			}
		case IDBlock:
			blockBody = payload
			blockOff = child.HeadOff
			found = true
		}
	}
	if !found {
		return nil
	}
	if len(blockBody) > int(lim.MaxBlockSize) {
		return &posError{err: ErrTooLarge, off: blockOff}
	}
	return appendBlock(doc, blockBody, clusterTc, blockOff, duration, lim)
}

func parseBlock(c *Cursor, doc *Document, el Element, clusterTc int64, duration int64, lim Limits) error {
	if el.Size > lim.MaxBlockSize {
		return &posError{err: ErrTooLarge, off: el.HeadOff}
	}
	body, err := c.ReadBody(el)
	if err != nil {
		return err
	}
	return appendBlock(doc, body, clusterTc, el.HeadOff, duration, lim)
}

// appendBlock decodes a (Simple)Block track number and relative timecode.
// The frame payload is retained only within the total-bytes budget; on
// overflow the run stops with a buffer-limit fault (backpressure signal).
func appendBlock(doc *Document, body []byte, clusterTc, off, durationTicks int64, lim Limits) error {
	if len(body) < 4 {
		return &posError{err: ErrTruncated, off: off}
	}
	track, n, ok := readBlockVint(body)
	if !ok {
		return &posError{err: ErrBadVint, off: off}
	}
	if len(body) < n+3 {
		return &posError{err: ErrTruncated, off: off}
	}
	rel := int16(uint16(body[n])<<8 | uint16(body[n+1]))
	flags := body[n+2]
	keyframe := flags&0x80 != 0
	tc := clusterTc + int64(rel)
	if tc < 0 {
		tc = 0
	}
	trackInfo, declared := doc.TrackByNumber(track)
	if !declared {
		doc.Faults.Addf(media.LevelFatal, media.CodeUnknownTrack, track, off,
			"block references undeclared track %d", track)
		doc.StopTrack(track)
		return nil
	}
	if doc.StoppedTracks[track] {
		return nil
	}
	if len(doc.Blocks) >= lim.MaxBlocks {
		doc.Faults.Addf(media.LevelFatal, media.CodeBufferLimit, track, off,
			"block count exceeds cap %d", lim.MaxBlocks)
		doc.StopTrack(track)
		doc.MarkTruncated()
		return nil
	}
	payload := body[n+3:]
	if doc.TotalBytes+int64(len(payload)) > lim.MaxTotalBytes {
		doc.Faults.Addf(media.LevelFatal, media.CodeBufferLimit, track, off,
			"retained bytes would exceed cap %d", lim.MaxTotalBytes)
		doc.StopTrack(track)
		doc.MarkTruncated()
		return nil
	}
	doc.TotalBytes += int64(len(payload))
	b := Block{
		Track:    track,
		Ticks:    tc,
		Keyframe: keyframe,
		Data:     append([]byte(nil), payload...),
		Offset:   off,
	}
	if durationTicks > 0 {
		b.DurationNs = durationTicks * doc.TimecodeScale
	}
	doc.Blocks = append(doc.Blocks, b)
	_ = trackInfo
	return nil
}

// readBlockVint decodes the block track number (a size-style vint) and
// returns its byte width.
func readBlockVint(b []byte) (uint64, int, bool) {
	if len(b) == 0 {
		return 0, 0, false
	}
	width, ok := vintWidth(b[0])
	if !ok || width > len(b) || width > 8 {
		return 0, 0, false
	}
	mask := byte(1)<<(8-uint(width)) - 1
	v := uint64(b[0] & mask)
	for i := 1; i < width; i++ {
		v = v<<8 | uint64(b[i])
	}
	return v, width, true
}
