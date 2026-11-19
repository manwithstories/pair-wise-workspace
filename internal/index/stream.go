package index

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"

	"github.com/abema/go-mp4"
)

// boxSpan locates a box's payload without decoding it.
type boxSpan struct {
	payloadOff int64
	payloadLen int64
}

// findBoxes returns the payload span of the first box matching path.
func findBoxes(f *os.File, path ...mp4.BoxType) (boxSpan, bool, error) {
	var found *mp4.BoxInfo
	// ReadBoxStructure descends only into boxes the handler expands, so the
	// requested path is walked explicitly and the final box's payload is never
	// decoded: verification streams it.
	_, err := mp4.ReadBoxStructure(f, func(h *mp4.ReadHandle) (interface{}, error) {
		depth := len(h.Path) - 1 // path includes the box itself as last element
		if depth >= len(path) || h.Path[depth] != path[depth] {
			return nil, nil
		}
		if depth == len(path)-1 {
			cp := h.BoxInfo
			found = &cp
			return nil, nil
		}
		return h.Expand()
	})
	if err != nil {
		return boxSpan{}, false, err
	}
	if found == nil {
		return boxSpan{}, false, nil
	}
	return boxSpan{
		payloadOff: int64(found.Offset + found.HeaderSize),
		payloadLen: int64(found.Size - found.HeaderSize),
	}, true, nil
}

// entryReader streams fixed-size big-endian entries from a box payload.
type entryReader struct {
	f      *os.File
	off    int64
	remain int64
	size   int64
	buf    []byte
	pos    int
}

func newEntryReader(f *os.File, span boxSpan, skip int64, size int64) *entryReader {
	return &entryReader{f: f, off: span.payloadOff + skip, remain: span.payloadLen - skip, size: size}
}

func (r *entryReader) next() ([]byte, bool, error) {
	if r.pos >= len(r.buf) {
		if r.remain < r.size {
			return nil, false, nil
		}
		n := int64(4096 / r.size * r.size)
		if r.remain < n {
			n = r.remain
		}
		if cap(r.buf) < int(n) {
			r.buf = make([]byte, n)
		}
		r.buf = r.buf[:n]
		if _, err := r.f.ReadAt(r.buf, r.off); err != nil && err != io.EOF {
			return nil, false, err
		}
		r.off += n
		r.remain -= n
		r.pos = 0
	}
	v := r.buf[r.pos : r.pos+int(r.size)]
	r.pos += int(r.size)
	return v, true, nil
}

// readU32At reads a big-endian uint32 at an absolute file offset.
func readU32At(f *os.File, off int64) (uint32, error) {
	var b [4]byte
	if _, err := f.ReadAt(b[:], off); err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint32(b[:]), nil
}

// tableSource streams the sample geometry of an index without materialising
// per-sample arrays, so verification runs in constant memory.
type tableSource struct {
	f *os.File

	stts *entryReader
	stsz *entryReader
	stsc *entryReader
	stco *entryReader
	co64 bool

	// current stts run
	sttsCount uint32
	sttsDelta uint32

	// stsz geometry
	sampleSize  uint32
	sampleCount uint64

	// stsc iteration
	stscLoaded bool
	stscFirst  uint32
	stscSPC    uint32
	stscNext   uint32
	haveNext   bool
	stscDone   bool
	peeked     []byte

	// chunk sequence
	chunkIndex uint32 // 1-based index of the chunk whose offset is loaded
	curOff     uint64
	curLeft    uint32

	time     int64
	produced uint64
}

func openTableSource(f *os.File) (*tableSource, error) {
	sttsSpan, ok, err := findBoxes(f, mp4.BoxTypeMoov(), mp4.BoxTypeTrak(), mp4.BoxTypeMdia(), mp4.BoxTypeMinf(), mp4.BoxTypeStbl(), mp4.BoxTypeStts())
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("index is missing stts")
	}
	stszSpan, ok, err := findBoxes(f, mp4.BoxTypeMoov(), mp4.BoxTypeTrak(), mp4.BoxTypeMdia(), mp4.BoxTypeMinf(), mp4.BoxTypeStbl(), mp4.BoxTypeStsz())
	if err != nil || !ok {
		return nil, fmt.Errorf("index is missing stsz")
	}
	stscSpan, ok, err := findBoxes(f, mp4.BoxTypeMoov(), mp4.BoxTypeTrak(), mp4.BoxTypeMdia(), mp4.BoxTypeMinf(), mp4.BoxTypeStbl(), mp4.BoxTypeStsc())
	if err != nil || !ok {
		return nil, fmt.Errorf("index is missing stsc")
	}
	stcoSpan, ok, err := findBoxes(f, mp4.BoxTypeMoov(), mp4.BoxTypeTrak(), mp4.BoxTypeMdia(), mp4.BoxTypeMinf(), mp4.BoxTypeStbl(), mp4.BoxTypeStco())
	co64 := false
	if err != nil {
		return nil, err
	}
	if !ok {
		stcoSpan, ok, err = findBoxes(f, mp4.BoxTypeMoov(), mp4.BoxTypeTrak(), mp4.BoxTypeMdia(), mp4.BoxTypeMinf(), mp4.BoxTypeStbl(), mp4.BoxTypeCo64())
		co64 = true
		if err != nil || !ok {
			return nil, fmt.Errorf("index is missing stco/co64")
		}
	}

	s := &tableSource{f: f, co64: co64}
	s.stts = newEntryReader(f, sttsSpan, 8, 8)
	s.stsc = newEntryReader(f, stscSpan, 8, 12)
	offSize := int64(4)
	if co64 {
		offSize = 8
	}
	s.stco = newEntryReader(f, stcoSpan, 8, offSize)

	// stsz has a variable layout: sample_size (4) then sample_count (4).
	sampleSize, err := readU32At(f, stszSpan.payloadOff+4)
	if err != nil {
		return nil, err
	}
	count, err := readU32At(f, stszSpan.payloadOff+8)
	if err != nil {
		return nil, err
	}
	s.sampleSize = sampleSize
	s.sampleCount = uint64(count)
	if sampleSize != 0 {
		s.stsz = nil
	} else {
		s.stsz = newEntryReader(f, stszSpan, 12, 4)
	}
	return s, nil
}

// next returns the offset, size and decode time of the next sample.
func (s *tableSource) next() (off uint64, size uint32, t int64, ok bool, err error) {
	if s.produced >= s.sampleCount {
		return 0, 0, 0, false, nil
	}
	if s.curLeft == 0 {
		// Load the next chunk's offset and sample count.
		if err := s.loadNextChunk(); err != nil {
			return 0, 0, 0, false, err
		}
	}
	sz := s.sampleSize
	if s.stsz != nil {
		v, ok, err := s.stsz.next()
		if err != nil {
			return 0, 0, 0, false, err
		}
		if !ok {
			return 0, 0, 0, false, fmt.Errorf("stsz table is shorter than sample_count")
		}
		sz = binary.BigEndian.Uint32(v)
	}
	off = s.curOff
	t = s.time
	// Advance.
	s.curOff += uint64(sz)
	s.curLeft--
	s.produced++
	if err := s.advanceTime(); err != nil {
		return 0, 0, 0, false, err
	}
	return off, sz, t, true, nil
}

func (s *tableSource) advanceTime() error {
	for s.sttsCount == 0 {
		v, ok, err := s.stts.next()
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("stts table covers fewer than %d samples", s.sampleCount)
		}
		s.sttsCount = binary.BigEndian.Uint32(v[0:4])
		s.sttsDelta = binary.BigEndian.Uint32(v[4:8])
	}
	s.sttsCount--
	s.time += int64(s.sttsDelta)
	return nil
}

func (s *tableSource) loadNextChunk() error {
	if !s.stscLoaded {
		if err := s.loadStscEntry(); err != nil {
			return err
		}
		s.stscLoaded = true
	}
	for {
		if s.haveNext && s.chunkIndex+1 >= s.stscNext {
			if err := s.loadStscEntry(); err != nil {
				return err
			}
			continue
		}
		break
	}
	v, ok, err := s.stco.next()
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("stco table is shorter than stsc requires")
	}
	if s.co64 {
		s.curOff = binary.BigEndian.Uint64(v)
	} else {
		s.curOff = uint64(binary.BigEndian.Uint32(v))
	}
	s.curLeft = s.stscSPC
	s.chunkIndex++
	// Never let the stsc run exceed the remaining sample budget.
	if remaining := s.sampleCount - s.produced; uint64(s.curLeft) > remaining {
		s.curLeft = uint32(remaining)
	}
	if s.curLeft == 0 {
		return fmt.Errorf("stsc describes no samples for chunk %d", s.chunkIndex)
	}
	return nil
}

// loadStscEntry consumes the current stsc entry and peeks the next one with a
// one-entry lookahead, so the end of a run is known without rewinding.
func (s *tableSource) loadStscEntry() error {
	var v []byte
	if s.peeked != nil {
		v, s.peeked = s.peeked, nil
	} else {
		var ok bool
		var err error
		v, ok, err = s.stsc.next()
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("stsc table ended before all samples were described")
		}
	}
	s.stscFirst = binary.BigEndian.Uint32(v[0:4])
	s.stscSPC = binary.BigEndian.Uint32(v[4:8])
	if s.stscFirst == 0 || s.stscSPC == 0 {
		return fmt.Errorf("stsc entry has a zero field")
	}
	// The first stsc entry must start at chunk 1.
	if s.chunkIndex == 0 && s.stscFirst != 1 {
		return fmt.Errorf("stsc starts at chunk %d, want 1", s.stscFirst)
	}
	nv, nok, nerr := s.stsc.next()
	if nerr != nil {
		return nerr
	}
	if nok {
		cp := append([]byte(nil), nv...)
		s.peeked = cp
		s.stscNext = binary.BigEndian.Uint32(cp[0:4])
		s.haveNext = true
	} else {
		s.haveNext = false
	}
	return nil
}
