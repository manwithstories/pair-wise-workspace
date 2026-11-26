package media

import (
	"bytes"
	"io"

	"medcheck/fault"
)

// prefixReader lets header resync replay bytes already removed from an
// underlying stream. The prefix is always smaller than HeaderSize.
type prefixReader struct {
	r      io.Reader
	prefix []byte
}

func (p *prefixReader) Read(b []byte) (int, error) {
	if len(p.prefix) > 0 {
		n := copy(b, p.prefix)
		p.prefix = p.prefix[n:]
		return n, nil
	}
	return p.r.Read(b)
}

func (p *prefixReader) readByte() (byte, error) {
	var one [1]byte
	for {
		n, err := p.Read(one[:])
		if n > 0 {
			return one[0], nil
		}
		if err != nil {
			return 0, err
		}
	}
}

// ScanReader streams fixed access-unit headers without retaining payloads.
// A corrupt/truncated header containing a later magic resynchronizes at that
// magic instead of consuming the later access unit.
func ScanReader(r io.Reader, visit func(AccessUnit, *HeaderError) error) error {
	pr := &prefixReader{r: r}
	var offset int64

	readByte := func() (byte, error) {
		b, err := pr.readByte()
		if err == nil {
			offset++
		}
		return b, err
	}
	findMagic := func() error {
		state := 0
		for {
			b, err := readByte()
			if err != nil {
				return err
			}
			if b == Magic[state] {
				state++
				if state == len(Magic) {
					return nil
				}
			} else if b == Magic[0] {
				state = 1
			} else {
				state = 0
			}
		}
	}

	for {
		if err := findMagic(); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		start := offset - int64(len(Magic))
		header := make([]byte, HeaderSize)
		copy(header, Magic)
		complete := true
		for i := len(Magic); i < HeaderSize; i++ {
			b, err := readByte()
			if err != nil {
				complete = false
				break
			}
			header[i] = b
		}
		if !complete {
			he := HeaderError{Offset: start, Kind: fault.TruncatedHeader, Err: ErrTruncatedHeader}
			if err := visit(AccessUnit{Offset: start}, &he); err != nil {
				return err
			}
			return nil
		}

		u, decodeErr := DecodeHeader(header)
		u.Offset = start
		if decodeErr != nil {
			kind := fault.BadCRC
			nextPos := -1
			if rel := bytes.Index(header[len(Magic):], []byte(Magic)); rel >= 0 {
				nextPos = len(Magic) + rel
				kind = fault.TruncatedHeader
				decodeErr = ErrTruncatedHeader
			}
			he := HeaderError{Offset: start, Kind: kind, Err: decodeErr}
			if err := visit(u, &he); err != nil {
				return err
			}
			if nextPos >= 0 {
				pr.prefix = append([]byte(nil), header[nextPos:]...)
				offset -= int64(len(pr.prefix))
				continue
			}
			// CRC-only corruption normally leaves PayloadLen usable. Discard
			// the payload; EOF here ends the stream.
			n, _ := io.CopyN(io.Discard, pr, int64(u.PayloadLen))
			offset += n
			continue
		}

		if err := visit(u, nil); err != nil {
			return err
		}
		n, err := io.CopyN(io.Discard, pr, int64(u.PayloadLen))
		offset += n
		if err != nil && err != io.EOF {
			return err
		}
	}
}
