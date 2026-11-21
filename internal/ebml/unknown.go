package ebml

import (
	"fmt"

	"archivecheck.local/archivecheck/internal/fault"
)

// Globals lists the EBML header element IDs that may terminate an
// unknown-size Segment or Cluster. Per the Matroska specification these
// top-level elements may appear after a Segment, so encountering one while
// inside an unknown-size Segment ends the Segment rather than corrupting it.
var Globals = map[uint32]bool{
	IDEBML: true,
	IDVoid: false, // Void is a normal child and never terminates a parent.
}

// TopLevel lists elements that may follow a Segment at the file level.
var TopLevel = map[uint32]bool{
	IDEBML: true,
}

// ClusterChild lists elements that may follow a Cluster's children.
var ClusterChild = map[uint32]bool{
	IDCluster: true, IDTracks: true, IDInfo: true, IDCues: true, IDSeekHead: true,
	IDTags: true, IDChapters: true, IDAttachments: true, IDEBML: true,
}

// Context describes the scope an unknown-size element lives in.
type Context struct {
	// ParentIDs are the IDs of the enclosing elements, outermost first.
	ParentIDs []uint32
	// Limit is the exclusive end of the enclosing scope (input size at root).
	Limit int64
	// SiblingStartsRecorded is set when a sibling element at the same level
	// means the current unknown-size element must end.
	AllowSiblingTermination bool
}

// BoundResult describes how an unknown-size element's end was determined.
type BoundResult struct {
	// End is the exclusive end offset of the element.
	End int64
	// Reason explains the chosen boundary.
	Reason string
	// SiblingID is the element that terminated the scope, if any.
	SiblingID uint32
}

// ResolveUnknown determines where an unknown-size element ends.
//
// Matroska allows an unknown-size Segment and Clusters. The end is decided in
// this order:
//  1. a sibling/global element that the format permits at the parent level
//     terminates the element (the header has not been consumed);
//  2. otherwise the element runs to the parent's limit;
//  3. if that limit is beyond the input, the parent's own boundary is used
//     when it is known, else parsing fails with the first illegal offset.
//
// nextID is the ID of the element that followed this one, or 0 when the
// reader reached the limit. nextOffset is where that element starts. The
// returned End is where this element's data stops, so the caller can re-seek
// to nextOffset and continue with the sibling.
func ResolveUnknown(ctx Context, elem Element, nextID uint32, nextOffset int64, sourceSize int64) (BoundResult, error) {
	if !elem.UnknownSize {
		return BoundResult{End: elem.End, Reason: "declared size"}, nil
	}
	limit := ctx.Limit
	if limit <= 0 || limit > sourceSize {
		limit = sourceSize
	}
	terminates := false
	switch {
	case elem.ID == IDSegment:
		terminates = TopLevel[nextID] || nextID == IDEBML
	case elem.ID == IDCluster:
		terminates = ClusterChild[nextID] && nextID != IDCluster
	default:
		if ctx.AllowSiblingTermination && nextID != 0 {
			terminates = true
		}
	}
	if terminates && nextOffset >= elem.DataOffset && nextOffset <= limit {
		return BoundResult{
			End:       nextOffset,
			Reason:    fmt.Sprintf("%s terminated by sibling %s", elem.Name(), Name(nextID)),
			SiblingID: nextID,
		}, nil
	}
	if nextID == 0 {
		// Reader reached the enclosing limit or the end of input.
		return BoundResult{End: limit, Reason: "unknown-size element runs to parent limit"}, nil
	}
	if ctx.AllowSiblingTermination {
		if nextOffset >= elem.DataOffset && nextOffset <= limit {
			return BoundResult{
				End:       nextOffset,
				Reason:    fmt.Sprintf("sibling %s ends unknown-size element", Name(nextID)),
				SiblingID: nextID,
			}, nil
		}
	}
	return BoundResult{}, fault.AtElement(fault.StageParse, nextOffset, elem.Name(),
		fmt.Errorf("%w: element followed by %s before any provable boundary", ErrBoundary, Name(nextID)))
}
