package ingest

import (
	"fmt"
	"io"

	"github.com/abema/go-mp4"

	"relay/internal/media"
)

// ReadEvent is one outcome of reading the next fragment from a stream.
// Exactly one of Fragment or Fault is set. Faults with Isolation()==true
// mean the fragment bytes were skipped and the stream is positioned at the
// next candidate fragment.
type ReadEvent struct {
	Fragment *Fragment
	Fault    *media.Fault
	Offset   int64
}

// Reader streams top-level moof/mdat pairs from an io.ReadSeeker. It keeps
// only one fragment's bytes in memory at a time and reports truncation at
// the first erroneous byte offset.
type Reader struct {
	r       io.ReadSeeker
	source  string
	pos     int64
	size    int64
	started bool
}

// NewReader opens a fragment stream. size is the total byte length of the
// underlying content (used to detect a box that declares a length past EOF).
func NewReader(r io.ReadSeeker, source string, size int64) *Reader {
	return &Reader{r: r, source: source, size: size}
}

// Next returns the next read event, or io.EOF when the stream is exhausted.
func (rd *Reader) Next() (*ReadEvent, error) {
	for {
		if rd.pos >= rd.size {
			return nil, io.EOF
		}
		if _, err := rd.r.Seek(rd.pos, io.SeekStart); err != nil {
			return nil, err
		}
		h, err := ReadBoxHeader(rd.r, rd.pos)
		if err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return &ReadEvent{
					Offset: rd.pos,
					Fault:  media.NewFault(media.FaultTruncated, 0, 0, rd.pos,
						"trailing bytes do not form a box header"),
				}, nil
			}
			if f, ok := err.(*media.Fault); ok {
				// A malformed box gives no reliable boundary, so the remainder
				// of the stream is quarantined and the reader drains to EOF.
				rd.pos = rd.size
				return &ReadEvent{Offset: rd.pos, Fault: f}, nil
			}
			return nil, err
		}
		if h.Size == 0 {
			return nil, media.NewFault(media.FaultBadLength, 0, 0, h.Offset,
				"top-level fragment box must declare an explicit size")
		}
		end := h.Offset + int64(h.Size)
		if end > rd.size {
			fault := media.NewFault(media.FaultTruncated, 0, 0, rd.size,
				fmt.Sprintf("box %s at %d declares %d bytes past EOF %d",
					h.Type, h.Offset, h.Size, rd.size))
			rd.pos = rd.size
			return &ReadEvent{Offset: h.Offset, Fault: fault}, nil
		}
		if h.Type != mp4.BoxTypeMoof() {
			// Skip non-fragment top-level boxes (ftyp, free, ...) cleanly.
			rd.pos = end
			continue
		}
		return rd.readFragment(h, end)
	}
}

func (rd *Reader) readFragment(moofH BoxHeader, moofEnd int64) (*ReadEvent, error) {
	if _, err := rd.r.Seek(moofEnd, io.SeekStart); err != nil {
		return nil, err
	}
	mdatH, err := ReadBoxHeader(rd.r, moofEnd)
	if err != nil {
		return &ReadEvent{
			Offset: moofH.Offset,
			Fault: media.NewFault(media.FaultTruncated, 0, 0, moofEnd,
				"mdat header missing or truncated after moof"),
		}, nil
	}
	if mdatH.Type != mp4.BoxTypeMdat() {
		// Moof without a paired mdat: consume the moof alone and isolate.
		rd.pos = moofEnd
		return &ReadEvent{
			Offset: moofH.Offset,
			Fault: media.NewFault(media.FaultStructure, 0, 0, mdatH.Offset,
				"moof not followed by mdat, got "+mdatH.Type.String()),
		}, nil
	}
	fragEnd := mdatH.Offset + int64(mdatH.Size)
	if mdatH.Size == 0 || fragEnd > rd.size {
		rd.pos = rd.size
		return &ReadEvent{
			Offset: moofH.Offset,
			Fault: media.NewFault(media.FaultTruncated, 0, 0, moofEnd,
				"mdat extends past EOF"),
		}, nil
	}

	raw := make([]byte, fragEnd-moofH.Offset)
	if _, err := rd.r.Seek(moofH.Offset, io.SeekStart); err != nil {
		return nil, err
	}
	if _, err := io.ReadFull(rd.r, raw); err != nil {
		return nil, media.NewFault(media.FaultTruncated, 0, 0, moofH.Offset,
			"fragment bytes unreadable")
	}
	frag, perr := ParseFragment(rd.source, moofH.Offset, raw)
	rd.pos = fragEnd
	if perr != nil {
		f, _ := perr.(*media.Fault)
		if f == nil {
			return nil, perr
		}
		return &ReadEvent{Offset: moofH.Offset, Fault: f}, nil
	}
	return &ReadEvent{Offset: moofH.Offset, Fragment: frag}, nil
}

// Position returns the current absolute read offset.
func (rd *Reader) Position() int64 { return rd.pos }
