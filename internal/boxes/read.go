package boxes

import (
	"io"
	"os"

	"github.com/abema/go-mp4"
)

type boxVisitor func(bi *mp4.BoxInfo) error

// forChildren iterates direct children inside [payloadStart, parentEnd).
func forChildren(f io.ReadSeeker, payloadStart, parentEnd int64, visit boxVisitor) error {
	pos := payloadStart
	for pos < parentEnd {
		if _, err := f.Seek(pos, io.SeekStart); err != nil {
			return err
		}
		bi, err := mp4.ReadBoxInfo(f)
		if err != nil {
			return &boxScanError{offset: pos, kind: "short_box_header", err: err}
		}
		start := int64(bi.Offset)
		headerSize := int64(bi.HeaderSize)
		if bi.Size > uint64(parentEnd-start) {
			return &boxScanError{offset: parentEnd, kind: "box_length_out_of_bounds"}
		}
		size := int64(bi.Size)
		end, err := safeEnd(start, size)
		if err != nil || size < headerSize || end > parentEnd {
			return &boxScanError{offset: start, kind: "invalid_box_length"}
		}
		if err := visit(bi); err != nil {
			return err
		}
		pos = end
	}
	return nil
}

type boxScanError struct {
	fragment uint32
	offset   int64
	kind     string
	err      error
}

func (e *boxScanError) Error() string {
	if e.err != nil {
		return e.kind + ": " + e.err.Error()
	}
	return e.kind
}
func (e *boxScanError) Unwrap() error { return e.err }

func readPayload(f *os.File, bi *mp4.BoxInfo, maxPayload int64) ([]byte, int64, error) {
	start := int64(bi.Offset)
	headerSize := int64(bi.HeaderSize)
	payloadSize := int64(bi.Size) - headerSize
	if payloadSize < 0 || payloadSize > maxPayload {
		return nil, start, &boxScanError{offset: start, kind: "box_too_large"}
	}
	if _, err := bi.SeekToPayload(f); err != nil {
		return nil, start, err
	}
	buf := make([]byte, payloadSize)
	if _, err := io.ReadFull(f, buf); err != nil {
		return nil, start + headerSize, &boxScanError{offset: start + headerSize, kind: "truncated_box_payload", err: err}
	}
	return buf, start, nil
}
