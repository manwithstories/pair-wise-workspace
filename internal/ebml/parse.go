package ebml

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"avcheck/internal/media"
)

// Parse reads a complete or truncated Matroska/WebM stream. It never returns
// an allocation error caused by a malicious size: every declared length is
// checked against Limits before any buffer is created. Container damage is
// accumulated in doc.Faults; the returned error is non-nil only when no
// usable document could be produced.
func Parse(r io.Reader, lim Limits) (*Document, error) {
	if err := lim.Validate(); err != nil {
		return nil, err
	}
	doc := NewDocument()
	c := NewCursor(r, lim)
	if err := parseEBMLHeader(c, doc); err != nil {
		return doc, finishAbort(doc, err)
	}
	if err := parseSegment(c, doc, lim); err != nil {
		return doc, finishAbort(doc, err)
	}
	doc.Finalize()
	return doc, nil
}

// finishAbort records the terminal fault and marks a tail truncation so the
// caller knows no final manifest may be committed.
func finishAbort(doc *Document, err error) error {
	var f media.Fault
	if errors.As(err, &f) {
		doc.Faults.Add(f)
	} else {
		off := errorOffset(err)
		doc.Faults.Add(faultFor(err, off, 0))
	}
	if errors.Is(err, ErrTruncatedVint) || errors.Is(err, ErrTruncatedElement) || errors.Is(err, ErrTruncated) {
		doc.MarkTruncated()
	}
	return doc.Faults
}

// errorOffset extracts the absolute byte offset attached by the cursor.
func errorOffset(err error) int64 {
	var p interface{ Offset() int64 }
	if errors.As(err, &p) {
		return p.Offset()
	}
	return 0
}

func parseEBMLHeader(c *Cursor, doc *Document) error {
	headOff := c.Offset()
	el, err := c.ReadElement()
	if err != nil {
		return err
	}
	if el.ID != IDEBML {
		return fmt.Errorf("%w: expected EBML header 0x1A45DFA3, got 0x%X at offset %d",
			ErrBadVint, headOff, el.ID)
	}
	body, err := c.ReadBody(el)
	if err != nil {
		return err
	}
	sub := NewCursorAt(bytes.NewReader(body), c.lim, el.BodyOff)
	for {
		subEl, err := sub.ReadElement()
		if err != nil {
			if isEOF(err) {
				break
			}
			return err
		}
		payload, err := sub.ReadBody(subEl)
		if err != nil {
			return err
		}
		if subEl.ID == IDDocType {
			doc.DocType = trimZeros(payload)
		}
	}
	if doc.DocType != "matroska" && doc.DocType != "webm" {
		return fmt.Errorf("%w: unsupported DocType %q at offset %d", ErrBadVint, doc.DocType, headOff)
	}
	return nil
}

func parseSegment(c *Cursor, doc *Document, lim Limits) error {
	el, err := c.ReadElement()
	if err != nil {
		if isEOF(err) {
			return &posError{err: ErrTruncatedElement, off: c.Offset()}
		}
		return err
	}
	if el.ID != IDSegment {
		return fmt.Errorf("%w: expected segment 0x18538067 at offset %d, got 0x%X",
			ErrBadVint, el.HeadOff, el.ID)
	}
	end := int64(-1)
	if !el.Unknown {
		end = el.BodyOff + el.Size
	}
	return parseSegmentChildren(c, doc, lim, end)
}

func parseSegmentChildren(c *Cursor, doc *Document, lim Limits, end int64) error {
	for {
		if end >= 0 && c.Offset() >= end {
			return nil
		}
		id, err := c.PeekID()
		if err != nil {
			if isEOF(err) {
				if end >= 0 {
					return &posError{err: ErrTruncatedElement, off: c.Offset()}
				}
				return nil
			}
			return err
		}
		_ = id
		child, err := c.ReadElement()
		if err != nil {
			return err
		}
		if err := checkExtent(child, end); err != nil {
			return err
		}
		switch child.ID {
		case IDInfo:
			if err := parseInfo(c, doc, child); err != nil {
				return err
			}
		case IDTracks:
			if err := parseTracks(c, doc, child, lim); err != nil {
				return err
			}
		case IDCluster:
			if err := parseCluster(c, doc, child, lim, end); err != nil {
				return err
			}
		default:
			if child.Unknown {
				return fmt.Errorf("%w: unknown-size element 0x%X at offset %d", ErrTooLarge, child.ID, child.HeadOff)
			}
			if err := c.Skip(child.Size); err != nil {
				return err
			}
		}
	}
}
