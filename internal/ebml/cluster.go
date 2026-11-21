package ebml

import (
	"bufio"
	"bytes"
	"io"

	"mkvarchive/internal/model"
)

// SegmentMeta holds the verbatim metadata sections an archival writer must
// preserve, plus the parsed timestamp scale.
type SegmentMeta struct {
	TimestampScale uint64 // raw TimestampScale; default 1,000,000 when absent
	InfoData       []byte // raw SegmentInfo payload
	TracksData     []byte // raw Tracks payload
	TracksOff      int64
}

// Scanner streams blocks out of a Matroska/WebM stream while preserving
// source offsets, tolerating unknown-length Segment/Cluster elements and
// stopping cleanly at the first damage so confirmed blocks stay usable.
type Scanner struct {
	br   *bufio.Reader
	r    *Reader
	meta SegmentMeta

	clusterTC  int64
	clusterOff int64
	clusterEnd int64 // absolute end of current known-length cluster, else -1
	inCluster  bool

	firstErr *model.ArchiveError
	scanErr  error
	finished bool
}

// NewScanner consumes the EBML header and Segment header and positions the
// stream at the first segment child.
func NewScanner(src io.Reader) (*Scanner, error) {
	br := bufio.NewReaderSize(src, 64*1024)
	s := &Scanner{br: br, r: NewReader(br), clusterEnd: -1,
		meta: SegmentMeta{TimestampScale: 1_000_000}}

	hdr, err := s.r.Next()
	if err != nil {
		return nil, err
	}
	if hdr.ID != model.IDEBMLHeader {
		return nil, model.NewError("EBML_BAD_HEADER",
			"stream does not start with an EBML header element", hdr.HeaderOff)
	}
	if err = s.r.Skip(hdr); err != nil {
		return nil, err
	}
	seg, err := s.r.Next()
	if err != nil {
		return nil, err
	}
	if seg.ID != model.IDSegment {
		return nil, model.NewError("EBML_BAD_HEADER",
			"EBML header is not followed by a Segment element", seg.HeaderOff)
	}
	if err = s.loadSegmentMetadata(seg); err != nil {
		return nil, err
	}
	return s, nil
}

// Meta returns the captured segment metadata after NewScanner succeeds.
func (s *Scanner) Meta() SegmentMeta { return s.meta }

// loadSegmentMetadata walks segment children up to the first Cluster,
// capturing SegmentInfo and Tracks verbatim. Unknown-length metadata
// masters are rejected because they would swallow the whole segment.
func (s *Scanner) loadSegmentMetadata(seg *Element) error {
	var segmentEnd int64 = -1
	if seg.Size != UnknownSize {
		segmentEnd = seg.DataOff + seg.Size
	}
	for {
		if segmentEnd >= 0 && s.r.Offset() >= segmentEnd {
			s.finished = true
			return nil
		}
		if s.r.Offset() > 0 && s.peekID() == model.IDCluster {
			return nil
		}
		e, err := s.r.Next()
		if err == io.EOF {
			s.finished = true
			return nil
		}
		if err != nil {
			if ae, ok := err.(*model.ArchiveError); ok {
				s.firstErr = ae
			}
			s.scanErr = err
			return nil // metadata damage surfaces on the first Next call
		}
		switch e.ID {
		case model.IDSegmentInfo:
			if e.Size == UnknownSize {
				return model.NewError("EBML_UNKNOWN_LENGTH",
					"unknown-length SegmentInfo is not archivable", e.HeaderOff)
			}
			data, err := s.r.ReadData(e)
			if err != nil {
				return err
			}
			s.meta.InfoData = append([]byte(nil), data...)
			if err = s.parseInfo(data); err != nil {
				return err
			}
		case model.IDTracks:
			if e.Size == UnknownSize {
				return model.NewError("EBML_UNKNOWN_LENGTH",
					"unknown-length Tracks is not archivable", e.HeaderOff)
			}
			data, err := s.r.ReadData(e)
			if err != nil {
				return err
			}
			s.meta.TracksData = append([]byte(nil), data...)
			s.meta.TracksOff = e.HeaderOff
		case model.IDCluster:
			// Scanner.Next owns cluster walking; nothing to consume here.
			return s.enterCluster(e)
		default:
			if e.Size == UnknownSize {
				return model.NewError("EBML_UNKNOWN_LENGTH",
					"unknown-length top-level segment child", e.HeaderOff)
			}
			if err = s.r.Skip(e); err != nil {
				return err
			}
		}
	}
}

// peekID reads the next element identifier without consuming bytes.
func (s *Scanner) peekID() uint32 {
	first, err := s.br.Peek(1)
	if err != nil || len(first) == 0 {
		return 0
	}
	width := 0
	for w := 1; w <= 4; w++ {
		if first[0]&markerMasks[w] != 0 {
			width = w
			break
		}
	}
	if width == 0 {
		return 0
	}
	raw, err := s.br.Peek(width)
	if err != nil {
		return 0
	}
	v, err := ReadElementID(bytes.NewReader(append([]byte(nil), raw...)), 0)
	if err != nil {
		return 0
	}
	return uint32(v.Value)
}

func (s *Scanner) enterCluster(e *Element) error {
	s.inCluster = true
	s.clusterOff = e.HeaderOff
	s.clusterTC = 0
	if e.Size == UnknownSize {
		s.clusterEnd = -1
	} else {
		s.clusterEnd = e.DataOff + e.Size
	}
	return nil
}

// FirstError returns the first byte-anchored parse error encountered, or nil.
func (s *Scanner) FirstError() *model.ArchiveError { return s.firstErr }

// Next returns the next fully confirmed block. It returns io.EOF at a clean
// segment end; after damage it returns the anchored error and every block
// emitted before that point remains valid.
func (s *Scanner) Next() (*Block, error) {
	if s.scanErr != nil {
		return nil, s.scanErr
	}
	if s.finished {
		return nil, io.EOF
	}
	for {
		if s.inCluster && s.clusterEnd >= 0 && s.r.Offset() >= s.clusterEnd {
			s.inCluster = false
		}
		// An unknown-length cluster ends where another cluster begins.
		if s.inCluster && s.clusterEnd < 0 && s.peekID() == model.IDCluster {
			s.inCluster = false
		}
		if !s.inCluster {
			e, err := s.r.Next()
			if err == io.EOF {
				s.finished = true
				return nil, io.EOF
			}
			if err != nil {
				return nil, s.record(err)
			}
			if e.ID != model.IDCluster {
				return nil, s.record(model.NewError("EBML_UNEXPECTED_ELEMENT",
					"non-cluster element after metadata section", e.HeaderOff))
			}
			if err = s.enterCluster(e); err != nil {
				return nil, err
			}
		}
		e, err := s.r.Next()
		if err != nil {
			return nil, s.record(err)
		}
		switch e.ID {
		case model.IDClusterTimecode:
			data, err := s.r.ReadData(e)
			if err != nil {
				return nil, s.record(err)
			}
			s.clusterTC = int64(DecodeUnsigned(data))
		case model.IDSimpleBlock:
			return s.readBlock(e, model.IDSimpleBlock)
		case model.IDBlockGroup:
			blk, err := s.readBlockGroup(e)
			if err != nil {
				return nil, s.record(err)
			}
			if blk != nil {
				return blk, nil
			}
		default:
			if e.Size == UnknownSize {
				return nil, s.record(model.NewError("EBML_UNKNOWN_LENGTH",
					"unknown-length cluster child", e.HeaderOff))
			}
			if err = s.r.Skip(e); err != nil {
				return nil, s.record(err)
			}
		}
	}
}

func (s *Scanner) readBlock(e *Element, id uint32) (*Block, error) {
	data, err := s.r.ReadData(e)
	if err != nil {
		return nil, s.record(err)
	}
	return buildBlock(e, data, s.clusterTC, id)
}

func (s *Scanner) record(err error) error {
	if ae, ok := err.(*model.ArchiveError); ok && s.firstErr == nil {
		s.firstErr = ae
	}
	s.scanErr = err
	return err
}

func (s *Scanner) parseInfo(infoData []byte) error {
	cr := NewReader(bytes.NewReader(infoData))
	for {
		e, err := cr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if e.ID == model.IDTimestampScale {
			data, err := cr.ReadData(e)
			if err != nil {
				return err
			}
			s.meta.TimestampScale = DecodeUnsigned(data)
		} else if err = cr.Skip(e); err != nil {
			return err
		}
	}
}
