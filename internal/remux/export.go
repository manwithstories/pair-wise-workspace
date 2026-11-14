// Package remux exports a presentation-time interval into a self-contained MP4
// by copying sample bytes verbatim (no re-encoding) and rewriting the sample
// tables. Sample data is streamed through a bounded buffer so memory never
// exceeds roughly twice the exported sample bytes.
package remux

import (
	"bytes"
	"encoding/binary"
	"io"
	"os"

	"mp4replay/internal/boxscan"
	"mp4replay/internal/mp4atom"
	"mp4replay/internal/report"
)

// Span identifies a contiguous sample interval on one track.
type Span struct {
	Track *mp4atom.Track
	Start int // inclusive sample index
	End   int // exclusive sample index
}

// copyBufSize bounds the streaming copy buffer.
const copyBufSize = 64 * 1024

func wU32(b *bytes.Buffer, v uint32) {
	var x [4]byte
	binary.BigEndian.PutUint32(x[:], v)
	b.Write(x[:])
}

func wU64(b *bytes.Buffer, v uint64) {
	var x [8]byte
	binary.BigEndian.PutUint64(x[:], v)
	b.Write(x[:])
}

func wBox(b *bytes.Buffer, typ string, payload []byte) {
	wU32(b, uint32(8+len(payload)))
	b.WriteString(typ)
	b.Write(payload)
}

func wContainer(b *bytes.Buffer, typ string, kids ...[]byte) {
	var body bytes.Buffer
	for _, k := range kids {
		body.Write(k)
	}
	wBox(b, typ, body.Bytes())
}

func fullBox(b *bytes.Buffer, ver uint8, flags uint32) {
	b.WriteByte(ver)
	b.Write([]byte{byte(flags >> 16), byte(flags >> 8), byte(flags)})
}

func box(typ string, payload []byte) []byte {
	var b bytes.Buffer
	wBox(&b, typ, payload)
	return b.Bytes()
}

// Export writes spans into outPath. Every span must begin on a sync sample.
func Export(m *mp4atom.Movie, spans []Span, outPath string) error {
	for _, sp := range spans {
		if sp.Start >= sp.End || sp.Start < 0 || sp.End > len(sp.Track.Samples) {
			return report.NewError(report.ExitArgs, "export", "invalid span")
		}
		if !sp.Track.Samples[sp.Start].Sync {
			return report.NewError(report.ExitBoxInvalid, "export",
				"export must start on a sync sample")
		}
	}
	ftyp := boxscan.FindFirst(m.File.Roots, "ftyp")
	// Pass 1: build moov with zero offsets to learn its on-disk size.
	dummy, err := buildMoov(m, spans, nil)
	if err != nil {
		return err
	}
	var headerLen int
	if ftyp != nil {
		headerLen += int(ftyp.Size)
	}
	headerLen += len(dummy)
	mdatDataBase := int64(headerLen) + 8
	// Pass 2: assign real offsets relative to the mdat payload start.
	offsets := assignOffsets(spans, mdatDataBase)
	header, err := buildMoov(m, spans, offsets)
	if err != nil {
		return err
	}
	out, err := os.OpenFile(outPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return report.NewError(report.ExitFileRead, "export", err.Error())
	}
	defer out.Close()
	if ftyp != nil {
		if err := writeRawBox(m, out, ftyp); err != nil {
			return err
		}
	}
	if _, err := out.Write(header); err != nil {
		return report.NewError(report.ExitFileRead, "export", err.Error())
	}
	return writeMdat(m, out, spans)
}

// assignOffsets computes absolute file offsets for every exported sample.
func assignOffsets(spans []Span, base int64) map[int][]int64 {
	cursor := base
	out := make(map[int][]int64, len(spans))
	for ti, sp := range spans {
		list := make([]int64, 0, sp.End-sp.Start)
		for i := sp.Start; i < sp.End; i++ {
			list = append(list, cursor)
			cursor += int64(sp.Track.Samples[i].Size)
		}
		out[ti] = list
	}
	return out
}

func writeRawBox(m *mp4atom.Movie, w io.Writer, b *boxscan.Box) error {
	buf := make([]byte, copyBufSize)
	ra := m.ReaderAt()
	remaining := b.Size
	pos := b.Start
	for remaining > 0 {
		chunk := remaining
		if chunk > int64(len(buf)) {
			chunk = int64(len(buf))
		}
		if _, err := ra.ReadAt(buf[:chunk], pos); err != nil && err != io.EOF {
			return report.NewError(report.ExitFileRead, "export", err.Error())
		}
		if _, err := w.Write(buf[:chunk]); err != nil {
			return report.NewError(report.ExitFileRead, "export", err.Error())
		}
		pos += chunk
		remaining -= chunk
	}
	return nil
}
