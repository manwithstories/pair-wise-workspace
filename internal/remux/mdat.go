package remux

import (
	"encoding/binary"
	"io"

	"mp4replay/internal/mp4atom"
	"mp4replay/internal/report"
)

// co64Patch records where one track's co64 payload starts and the span it maps.
type co64Patch struct {
	payloadOff int64 // absolute file offset of first co64 entry
	count      int
}

// writeMdat streams sample bytes and returns per-track co64 patch descriptors.
// The moov must already be written; mdat starts immediately after it.
func writeMdat(m *mp4atom.Movie, w io.WriteSeeker, spans []Span) error {
	pos, err := w.Seek(0, io.SeekCurrent)
	if err != nil {
		return report.NewError(report.ExitFileRead, "export", err.Error())
	}
	// mdat header (8 bytes) then contiguous samples, one chunk each.
	if _, err := w.Write([]byte{0, 0, 0, 0, 'm', 'd', 'a', 't'}); err != nil {
		return report.NewError(report.ExitFileRead, "export", err.Error())
	}
	dataStart := pos + 8
	cursor := dataStart
	buf := make([]byte, copyBufSize)
	ra := m.ReaderAt()
	var total int64
	for _, sp := range spans {
		for i := sp.Start; i < sp.End; i++ {
			s := sp.Track.Samples[i]
			if err := copySample(ra, w, buf, s.Offset, int64(s.Size)); err != nil {
				return err
			}
			cursor += int64(s.Size)
			total += int64(s.Size)
		}
	}
	// patch mdat size
	end := cursor
	if _, err := w.Seek(pos, io.SeekStart); err != nil {
		return report.NewError(report.ExitFileRead, "export", err.Error())
	}
	var sizeBuf [4]byte
	binary.BigEndian.PutUint32(sizeBuf[:], uint32(end-pos))
	if _, err := w.Write(sizeBuf[:]); err != nil {
		return report.NewError(report.ExitFileRead, "export", err.Error())
	}
	return nil
}

func copySample(ra io.ReaderAt, w io.Writer, buf []byte,
	off, size int64) error {
	remaining := size
	for remaining > 0 {
		chunk := remaining
		if chunk > int64(len(buf)) {
			chunk = int64(len(buf))
		}
		if _, err := ra.ReadAt(buf[:chunk], off); err != nil && err != io.EOF {
			return report.NewError(report.ExitFileRead, "export", err.Error())
		}
		if _, err := w.Write(buf[:chunk]); err != nil {
			return report.NewError(report.ExitFileRead, "export", err.Error())
		}
		off += chunk
		remaining -= chunk
	}
	return nil
}
