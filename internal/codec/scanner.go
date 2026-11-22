package codec

import (
	"bufio"
	"bytes"
	"io"

	"mediapack/internal/fault"
)

// annexBScanner incrementally splits an Annex B stream (H.264/H.265) or an
// Annex-B-framed AV1 stream into raw units. The window slides forward as
// units are consumed, so memory stays bounded by one unit plus readWindow
// regardless of total stream length.
type annexBScanner struct {
	r         *bufio.Reader
	base      int64 // absolute offset of buf[0]
	buf       []byte
	cur       int // offset of current unit's start code within buf; -1 initially
	scLen     int // current start-code length (3 or 4)
	maxNAL    int
	eof       bool
	exhausted bool // final unit already emitted
}

const readWindow = 64 * 1024

func newAnnexBScanner(r io.Reader, maxNAL int) *annexBScanner {
	return &annexBScanner{
		r:      bufio.NewReaderSize(r, readWindow),
		maxNAL: maxNAL,
		cur:    -1,
	}
}

// next returns the next raw unit and the absolute input offset of its first
// payload byte. io.EOF means no more units.
func (s *annexBScanner) next() (raw []byte, payloadOff int64, err error) {
	if s.exhausted {
		return nil, 0, io.EOF
	}
	if s.cur < 0 {
		if err = s.seekFirst(); err != nil {
			return nil, 0, err
		}
	}
	unitStart := s.cur + s.scLen
	idx, scLen, found := s.findStart(unitStart)
	for !found && !s.eof {
		if len(s.buf)-unitStart >= s.maxNAL {
			return nil, 0, fault.At(fault.KindCorruptInput, "parse",
				s.base+int64(unitStart)+int64(s.maxNAL), "NAL unit exceeds size limit")
		}
		if err = s.fill(); err != nil && err != io.EOF {
			return nil, 0, err
		}
		idx, scLen, found = s.findStart(unitStart)
	}
	if !found {
		return s.emitLast(unitStart)
	}
	payload := trimTrailingZeros(s.buf[unitStart:idx])
	if len(payload) == 0 {
		return nil, 0, fault.At(fault.KindCorruptInput, "parse",
			s.base+int64(idx), "empty NAL unit")
	}
	out := make([]byte, len(payload))
	copy(out, payload)
	off := s.base + int64(unitStart)
	s.slideTo(idx)
	s.scLen = scLen
	return out, off, nil
}

// emitLast handles the final unit, which runs to end of input.
func (s *annexBScanner) emitLast(unitStart int) ([]byte, int64, error) {
	payload := trimTrailingZeros(s.buf[unitStart:])
	if len(payload) == 0 {
		return nil, 0, fault.At(fault.KindCorruptInput, "parse",
			s.base+int64(unitStart), "empty NAL unit at end of stream")
	}
	out := make([]byte, len(payload))
	copy(out, payload)
	off := s.base + int64(unitStart)
	s.slideTo(len(s.buf))
	s.exhausted = true
	return out, off, nil
}

// seekFirst moves to the first start code, tolerating leading garbage.
func (s *annexBScanner) seekFirst() error {
	for {
		if i := bytes.Index(s.buf, []byte{0, 0, 1}); i >= 0 {
			s.cur, s.scLen = i, 3
			if i > 0 && s.buf[i-1] == 0 {
				s.cur, s.scLen = i-1, 4
			}
			return nil
		}
		if s.eof {
			return fault.At(fault.KindCorruptInput, "parse", s.base,
				"no Annex B start code found in input")
		}
		// Retain the last two bytes: a start code may straddle the window.
		keep := 2
		if len(s.buf) < keep {
			keep = len(s.buf)
		}
		drop := len(s.buf) - keep
		s.base += int64(drop)
		s.buf = append(s.buf[:0], s.buf[drop:]...)
		if err := s.fill(); err != nil && err != io.EOF {
			return err
		}
	}
}

// findStart locates a start code at or after from, expanding a leading zero
// into the four-byte form.
func (s *annexBScanner) findStart(from int) (idx, scLen int, found bool) {
	if from > len(s.buf) {
		return 0, 0, false
	}
	rel := bytes.Index(s.buf[from:], []byte{0, 0, 1})
	if rel < 0 {
		return 0, 0, false
	}
	i := from + rel
	if i > 0 && s.buf[i-1] == 0 {
		return i - 1, 4, true
	}
	return i, 3, true
}

// slideTo discards bytes before idx while keeping idx as the new window base.
func (s *annexBScanner) slideTo(idx int) {
	s.base += int64(idx)
	s.buf = append(s.buf[:0], s.buf[idx:]...)
	s.cur = 0
}

func (s *annexBScanner) fill() error {
	tmp := make([]byte, readWindow)
	n, err := s.r.Read(tmp)
	if n > 0 {
		s.buf = append(s.buf, tmp[:n]...)
	}
	if err == io.EOF {
		s.eof = true
		return io.EOF
	}
	return err
}

func trimTrailingZeros(b []byte) []byte {
	end := len(b)
	for end > 1 && b[end-1] == 0 {
		end--
	}
	return b[:end]
}
