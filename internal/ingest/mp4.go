// Package ingest demultiplexes MP4, MXF and RTP inputs into the unified
// event model. Each demuxer preserves sample bytes, track identity and both
// decoding and presentation timestamps, and reports length violations with a
// stable code, track and byte offset rather than silently truncating.
package ingest

import (
	"crypto/sha256"
	"fmt"
	"io"

	"github.com/abema/go-mp4"

	"mediameld/internal/model"
)

// boxNode is the minimal structural tree retained while parsing a track.
type boxNode struct {
	Type     string
	Offset   uint64
	Size     uint64
	Children []*boxNode
}

// rawMP4Track gathers the boxes belonging to one trak.
type rawMP4Track struct {
	id    uint32
	boxes []*boxNode
}

// DemuxMP4 parses a non-fragmented MP4 into tracks. The file is scanned once
// to build a structural tree, then the sample tables and display metadata are
// resolved per trak.
func DemuxMP4(r io.ReadSeeker, sourcePath string) ([]*model.Track, error) {
	roots, err := scanBoxes(r)
	if err != nil {
		return nil, err
	}
	var tracks []*model.Track
	for _, root := range roots {
		if root.Type != "moov" {
			continue
		}
		for _, trak := range root.Children {
			if trak.Type != "trak" {
				continue
			}
			t, err := parseTrak(r, trak, sourcePath)
			if err != nil {
				return nil, err
			}
			tracks = append(tracks, t)
		}
	}
	if len(tracks) == 0 {
		return nil, model.NewError(model.CodeTrackMissing, "no trak found in MP4")
	}
	return tracks, nil
}

// scanBoxes walks the container hierarchy, hardening every length against the
// enclosing box and the physical file size. A bad length is a hard error, not
// a truncation.
func scanBoxes(r io.ReadSeeker) ([]*boxNode, error) {
	end, err := r.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, err
	}
	nodes, err := scanRange(r, 0, uint64(end), nil)
	if err != nil {
		return nil, err
	}
	return nodes, nil
}

// containerTypes whose payload is a sequence of child boxes.
var containerTypes = map[string]bool{
	"moov": true, "trak": true, "mdia": true, "minf": true, "stbl": true,
	"dinf": true, "edts": true, "udta": true, "avc1": true, "mp4a": true,
	"mvex": true, "moof": true, "traf": true, "encv": true, "enca": true,
	"meta": true,
}

func scanRange(r io.ReadSeeker, start, end uint64, path []string) ([]*boxNode, error) {
	var out []*boxNode
	var off = start
	for off < end {
		remaining := end - off
		if remaining < 8 {
			if remaining == 0 {
				break
			}
			return nil, model.NewError(model.CodeMP4BoxLength,
				"trailing %d bytes cannot form a box header (path=%v)", remaining, path).WithOffset(int64(off))
		}
		if _, err := r.Seek(int64(off), io.SeekStart); err != nil {
			return nil, err
		}
		bi, err := mp4.ReadBoxInfo(r)
		if err != nil {
			return nil, model.WrapError(model.CodeMP4BoxLength, err, "cannot read box header (path=%v)", path).WithOffset(int64(off))
		}
		if bi.Size < bi.HeaderSize {
			return nil, model.NewError(model.CodeMP4BoxLength,
				"box %q size %d smaller than header %d (path=%v)", bi.Type.String(), bi.Size, bi.HeaderSize, path).WithOffset(int64(off))
		}
		if bi.Size > remaining {
			return nil, model.NewError(model.CodeMP4BoxLength,
				"box %q size %d overruns parent with %d bytes (path=%v)", bi.Type.String(), bi.Size, remaining, path).WithOffset(int64(off))
		}
		node := &boxNode{Type: bi.Type.String(), Offset: bi.Offset, Size: bi.Size}
		if containerTypes[node.Type] {
			parents := append(append([]string(nil), path...), node.Type)
			childStart := bi.Offset + bi.HeaderSize
			if node.Type == "meta" {
				childStart += 4 // full-box version/flags precede children
			}
			children, err := scanRange(r, childStart, bi.Offset+bi.Size, parents)
			if err != nil {
				return nil, err
			}
			node.Children = children
		}
		out = append(out, node)
		off = bi.Offset + bi.Size
	}
	return out, nil
}

func findChild(n *boxNode, typ string) *boxNode {
	for _, c := range n.Children {
		if c.Type == typ {
			return c
		}
	}
	return nil
}

func findAll(n *boxNode, typ string) []*boxNode {
	var out []*boxNode
	for _, c := range n.Children {
		if c.Type == typ {
			out = append(out, c)
		}
	}
	return out
}

func childPath(n *boxNode, types ...string) *boxNode {
	cur := n
	for _, t := range types {
		cur = findChild(cur, t)
		if cur == nil {
			return nil
		}
	}
	return cur
}

func parseTrak(r io.ReadSeeker, trak *boxNode, sourcePath string) (*model.Track, error) {
	tkhdNode := findChild(trak, "tkhd")
	mdhdNode := childPath(trak, "mdia", "mdhd")
	hdlrNode := childPath(trak, "mdia", "hdlr")
	stbl := childPath(trak, "mdia", "minf", "stbl")
	if tkhdNode == nil || mdhdNode == nil || stbl == nil {
		return nil, model.NewError(model.CodeInternal, "trak missing required boxes").WithOffset(int64(trak.Offset))
	}

	tkhd, err := unmarshalAt(r, tkhdNode, &mp4.Tkhd{})
	if err != nil {
		return nil, err
	}
	mdhd, err := unmarshalAt(r, mdhdNode, &mp4.Mdhd{})
	if err != nil {
		return nil, err
	}
	rot := rotationFromMatrix(tkhd.Matrix)
	t := &model.Track{
		ID:                fmt.Sprintf("%d", tkhd.TrackID),
		Timescale:         int64(mdhd.Timescale),
		SourcePath:        sourcePath,
		ContainerRotation: rot,
	}
	t.Display.Rotation = rot

	if hdlrNode != nil {
		hdlr, err := unmarshalAt(r, hdlrNode, &mp4.Hdlr{})
		if err != nil {
			return nil, err
		}
		switch string(hdlr.HandlerType[:]) {
		case "vide":
			t.Kind = model.KindVideo
		default:
			t.Kind = model.KindAudio
		}
	}
	stsd := findChild(stbl, "stsd")
	if stsd == nil {
		return nil, model.NewError(model.CodeTrackMissing, "trak %d lacks stsd", tkhd.TrackID)
	}
	if t.Kind == model.KindVideo {
		if err := parseVisualEntry(r, stsd, t); err != nil {
			return nil, err
		}
	} else {
		if err := parseAudioEntry(r, stsd, t); err != nil {
			return nil, err
		}
	}
	if err := parseSamples(r, stbl, t); err != nil {
		return nil, err
	}
	return t, nil
}

func unmarshalAt[T mp4.IBox](r io.ReadSeeker, n *boxNode, dst T) (T, error) {
	var zero T
	if n == nil {
		return zero, model.NewError(model.CodeInternal, "missing box")
	}
	if _, err := r.Seek(int64(n.Offset), io.SeekStart); err != nil {
		return zero, err
	}
	bi, err := mp4.ReadBoxInfo(r)
	if err != nil {
		return zero, model.WrapError(model.CodeMP4BoxLength, err, "box %s", n.Type).WithOffset(int64(n.Offset))
	}
	if _, err := bi.SeekToPayload(r); err != nil {
		return zero, err
	}
	if _, err := mp4.Unmarshal(r, bi.Size-bi.HeaderSize, dst, bi.Context); err != nil {
		return zero, model.WrapError(model.CodeMP4BoxLength, err, "malformed %s box", n.Type).WithOffset(int64(n.Offset))
	}
	return dst, nil
}

func hashRegion(r io.ReadSeeker, off, n int64) ([32]byte, error) {
	var sum [32]byte
	if _, err := r.Seek(off, io.SeekStart); err != nil {
		return sum, err
	}
	h := sha256.New()
	if _, err := io.CopyN(h, r, n); err != nil {
		return sum, model.WrapError(model.CodeMP4BoxLength, err, "sample payload truncated").WithOffset(off)
	}
	copy(sum[:], h.Sum(nil))
	return sum, nil
}

var _ = findAll

// rotationFromMatrix recovers a clockwise display rotation from a tkhd matrix
// at right-angle precision.
func rotationFromMatrix(m [9]int32) int {
	a, b := int64(m[0]), int64(m[1])
	switch {
	case a == 0 && b > 0:
		return 90
	case a == 0 && b < 0:
		return 270
	case a < 0 && b == 0:
		return 180
	}
	return 0
}
