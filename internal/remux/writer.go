package remux

import (
	"encoding/binary"
	"fmt"
	"io"

	"github.com/abema/go-mp4"

	"archivecheck.local/archivecheck/internal/fault"
)

// writer writes ISO-BMFF boxes with explicit size patching. go-mp4 marshals
// box payloads (leaf boxes and sample entries); container boxes are framed and
// sized here because the archive layout is fully determined by the plan.
type writer struct {
	w   io.WriteSeeker
	pos int64
}

func (b *writer) write(p []byte) error {
	n, err := b.w.Write(p)
	b.pos += int64(n)
	if err != nil {
		return fault.New(fault.StageRemux, fmt.Errorf("%w: %v", fault.ErrOutput, err))
	}
	if n != len(p) {
		return fault.New(fault.StageRemux, fmt.Errorf("%w: short write %d/%d", fault.ErrOutput, n, len(p)))
	}
	return nil
}

func (b *writer) seek(off int64) error {
	if _, err := b.w.Seek(off, io.SeekStart); err != nil {
		return fault.New(fault.StageRemux, err)
	}
	b.pos = off
	return nil
}

// box writes a container: 8-byte header (size, type), then body, then patches
// the size. Sizes larger than 4 GiB are rejected for non-mdat boxes; callers
// that may stream media use box64.
func (b *writer) box(typ string, body func() error) error {
	return b.frame(typ, 8, body)
}

// box64 always uses a 16-byte largesize header.
func (b *writer) box64(typ string, body func() error) error {
	return b.frame(typ, 16, body)
}

func (b *writer) frame(typ string, headerSize int64, body func() error) error {
	if len(typ) != 4 {
		return fault.New(fault.StageRemux, fmt.Errorf("%w: box type %q", fault.ErrMalformed, typ))
	}
	start := b.pos
	hdr := make([]byte, headerSize)
	copy(hdr[4:], typ)
	if headerSize == 16 {
		binary.BigEndian.PutUint32(hdr[0:], 1)
	}
	if err := b.write(hdr); err != nil {
		return err
	}
	if err := body(); err != nil {
		return err
	}
	end := b.pos
	size := end - start
	if headerSize == 8 && size > 0xFFFFFFFF {
		return fault.New(fault.StageRemux, fmt.Errorf("%w: box %s exceeds 4 GiB", fault.ErrOutput, typ))
	}
	if err := b.seek(start); err != nil {
		return err
	}
	if headerSize == 16 {
		binary.BigEndian.PutUint32(hdr[0:], 1)
		binary.BigEndian.PutUint64(hdr[8:], uint64(size))
	} else {
		binary.BigEndian.PutUint32(hdr[0:], uint32(size))
	}
	if err := b.write(hdr); err != nil {
		return err
	}
	return b.seek(end)
}

// payload marshals a go-mp4 box payload with no header.
func (b *writer) payload(box mp4.IImmutableBox) error {
	buf := &seekBuffer{w: b}
	if _, err := mp4.Marshal(buf, box, mp4.Context{}); err != nil {
		return fault.New(fault.StageRemux, fmt.Errorf("marshal %s: %w", box.GetType().String(), err))
	}
	return nil
}

// leaf writes a complete box from a go-mp4 payload.
func (b *writer) leaf(box mp4.IImmutableBox) error {
	return b.box(box.GetType().String(), func() error { return b.payload(box) })
}

// seekBuffer adapts writer to io.Writer for mp4.Marshal.
type seekBuffer struct{ w *writer }

func (s *seekBuffer) Write(p []byte) (int, error) {
	if err := s.w.write(p); err != nil {
		return 0, err
	}
	return len(p), nil
}

// rawBox writes a complete box whose payload is already encoded.
func (b *writer) rawBox(typ string, payload []byte) error {
	return b.box(typ, func() error { return b.write(payload) })
}

// fullBox writes version/flags followed by payload.
func (b *writer) fullBox(typ string, version byte, flags uint32, payload []byte) error {
	return b.box(typ, func() error {
		hdr := []byte{version, byte(flags >> 16), byte(flags >> 8), byte(flags)}
		if err := b.write(hdr); err != nil {
			return err
		}
		return b.write(payload)
	})
}
