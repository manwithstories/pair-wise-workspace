// Package inspect parses MP4 container structure and validates box
// hierarchy, sample durations and keyframe spacing. Truncated boxes
// are detected with a bounded raw walker before go-mp4 is asked to
// interpret the healthy structure.
package inspect

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/abema/go-mp4"

	"mediacheck/internal/model"
)

// BoxNode is one element of the validated box hierarchy.
type BoxNode struct {
	Type     string    `json:"type"`
	Path     string    `json:"path"`
	Offset   int64     `json:"offset"`
	Header   int64     `json:"header_size"`
	Size     int64     `json:"size"`
	Children []BoxNode `json:"children,omitempty"`
}

// BoxReport aggregates structure findings.
type BoxReport struct {
	Root      []BoxNode     `json:"root"`
	Faults    []model.Fault `json:"faults"`
	MDATBytes int64         `json:"mdat_bytes"`
	nextID    int
}

// RequiredContainer enforces the minimal broadcast hierarchy.
var RequiredContainer = [][]string{
	{"ftyp"},
	{"moov"},
	{"moov", "mvhd"},
	{"moov", "trak"},
	{"moov", "trak", "mdia"},
	{"moov", "trak", "mdia", "mdhd"},
	{"moov", "trak", "mdia", "minf"},
	{"moov", "trak", "mdia", "minf", "stbl"},
}

// containerTypes are boxes whose payload holds nested boxes.
var containerTypes = map[string]bool{
	"moov": true, "trak": true, "edts": true, "mdia": true,
	"minf": true, "dinf": true, "stbl": true, "mvex": true,
	"moof": true, "traf": true, "mfra": true, "udta": true,
}

// WalkBoxes performs a bounded raw scan. A declared size that runs
// past EOF is reported as a truncated-box fault carrying the box
// offset and the recoverable end boundary.
func WalkBoxes(r io.ReaderAt, total int64, limit int64) (*BoxReport, error) {
	if total <= 0 || total > limit {
		return nil, fmt.Errorf("inspect: input size %d outside (0,%d]", total, limit)
	}
	rep := &BoxReport{nextID: 1}
	rep.Root = rep.walkChildren(r, 0, total, "", 0)
	rep.checkHierarchy()
	return rep, nil
}

func (r *BoxReport) walkChildren(ra io.ReaderAt, start, end int64, parent string, depth int) []BoxNode {
	nodes := []BoxNode{}
	pos := start
	hdr := make([]byte, 16)
	for pos+8 <= end {
		if _, err := ra.ReadAt(hdr[:8], pos); err != nil {
			r.raise(model.KindTruncatedBox, "container", pos, end,
				fmt.Sprintf("box header unreadable under %s", parent))
			return nodes
		}
		size := int64(binary.BigEndian.Uint32(hdr[:4]))
		typ := string(bytes.TrimRight(hdr[4:8], "\x00"))
		hsize := int64(8)
		if size == 1 {
			if _, err := ra.ReadAt(hdr[8:16], pos+8); err != nil {
				r.raise(model.KindTruncatedBox, "container", pos, end,
					fmt.Sprintf("largesize box %s truncated", typ))
				return nodes
			}
			size = int64(binary.BigEndian.Uint64(hdr[8:16]))
			hsize = 16
		}
		if size == 0 { // box extends to end of file
			size = end - pos
		}
		if size < hsize || pos+size > end {
			r.raise(model.KindTruncatedBox, "container", pos, end,
				fmt.Sprintf("box %s declares %d bytes but %d remain",
					typ, size, end-pos))
			return nodes // stop this subtree; later siblings untrusted
		}
		node := BoxNode{
			Type: typ, Path: joinPath(parent, typ),
			Offset: pos, Header: hsize, Size: size,
		}
		if containerTypes[typ] && depth < 16 {
			node.Children = r.walkChildren(ra, pos+hsize, pos+size, node.Path, depth+1)
		}
		if typ == "mdat" {
			r.MDATBytes += size - hsize
		}
		nodes = append(nodes, node)
		pos += size
	}
	return nodes
}

func (r *BoxReport) checkHierarchy() {
	present := map[string]bool{}
	var collect func(nodes []BoxNode)
	collect = func(nodes []BoxNode) {
		for i := range nodes {
			present[nodes[i].Path] = true
			collect(nodes[i].Children)
		}
	}
	collect(r.Root)
	for _, path := range RequiredContainer {
		key := joinPath("", path...)
		if !present[key] {
			r.raise(model.KindTruncatedBox, "container", 0, 0,
				"missing required box path: "+key)
		}
	}
}

// ParseWithGoMp4 runs the go-mp4 structural reader over the healthy
// region. Its error is mapped onto the truncated-box fault family so
// callers keep one fault taxonomy.
func ParseWithGoMp4(r io.ReadSeeker) (*BoxReport, error) {
	rep := &BoxReport{nextID: 1}
	_, err := mp4.ReadBoxStructure(r, func(h *mp4.ReadHandle) (interface{}, error) {
		node := BoxNode{
			Type:   h.BoxInfo.Type.String(),
			Path:   pathString(h.Path),
			Offset: int64(h.BoxInfo.Offset),
			Header: int64(h.BoxInfo.HeaderSize),
			Size:   int64(h.BoxInfo.Size),
		}
		if node.Type == "mdat" {
			rep.MDATBytes += node.Size - node.Header
		}
		rep.Root = append(rep.Root, node)
		return h.Expand()
	})
	if err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
			rep.raise(model.KindTruncatedBox, "container", 0, 0,
				"go-mp4 hit unexpected EOF: "+err.Error())
			return rep, nil
		}
		return rep, err
	}
	return rep, nil
}

func (r *BoxReport) raise(k model.Kind, track string, start, end int64, detail string) {
	r.Faults = append(r.Faults, model.Fault{
		ID: r.nextID, Kind: k, Severity: model.SeverityError,
		Track: track, StartMs: start, EndMs: end, Detail: detail,
		Recovery: model.RecPending,
	})
	r.nextID++
}

func joinPath(parent string, parts ...string) string {
	out := parent
	for _, p := range parts {
		if out == "" {
			out = p
		} else {
			out += "/" + p
		}
	}
	return out
}

// pathString renders a go-mp4 BoxPath as a slash-joined string.
func pathString(p mp4.BoxPath) string {
	out := ""
	for _, t := range p {
		if out == "" {
			out = t.String()
		} else {
			out += "/" + t.String()
		}
	}
	return out
}
