// Package demux contains the three input adapters of the relay: FSEQ frame
// sequences, fragmented MP4 (ISO BMFF) and length-prefixed RTP logs. Every
// adapter validates framing, declared lengths and per-sample SHA-256 hashes
// and emits media.RawFrame values whose payload bytes are never modified.
package demux

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"

	"relay/internal/media"
)

// FSEQMagic marks a frame-sequence file; the trailing byte is the version.
var FSEQMagic = [5]byte{'F', 'S', 'E', 'Q', '1'}

const (
	fseqHeaderSize = 5 + 1 + 4 + 1 + 4 + 4 + 4 + 4 // magic+ver, source, kind...
	fseqRecordHdr  = 8 + 8 + 8 + 4                 // seq, ticks, refNS, size
)

// FSEQHeader is the capture metadata shared by every frame in one FSEQ file.
type FSEQHeader struct {
	SourceID    uint32
	Kind        media.Kind
	TimescaleHz uint32
	Width       uint32
	Height      uint32
	FrameCount  uint32
}

// WriteFSEQ serialises frames into the FSEQ1 container. The per-frame hash is
// computed here from the payload, so the writer is the single source of the
// checksums the reader later verifies.
func WriteFSEQ(w io.Writer, hdr FSEQHeader, frames []*media.RawFrame) error {
	hdr.FrameCount = uint32(len(frames))
	if uint32(len(frames)) > media.MaxTotalSamples {
		return media.NewError(media.ErrLimitExceeded, hdr.SourceID, "frame count over limit")
	}
	var buf [fseqHeaderSize]byte
	copy(buf[0:5], FSEQMagic[:])
	buf[5] = 1
	bin := binary.BigEndian
	bin.PutUint32(buf[6:10], hdr.SourceID)
	buf[10] = byte(hdr.Kind)
	bin.PutUint32(buf[11:15], hdr.TimescaleHz)
	bin.PutUint32(buf[15:19], hdr.Width)
	bin.PutUint32(buf[19:23], hdr.Height)
	bin.PutUint32(buf[23:27], hdr.FrameCount)
	if _, err := w.Write(buf[:]); err != nil {
		return err
	}
	for _, fr := range frames {
		if len(fr.Payload) > media.MaxFrameBytes {
			return media.NewError(media.ErrLimitExceeded, hdr.SourceID, "frame too large")
		}
		var rh [fseqRecordHdr]byte
		bin.PutUint64(rh[0:8], fr.Meta.Sequence)
		bin.PutUint64(rh[8:16], fr.Meta.ClockTicks)
		bin.PutUint64(rh[16:24], uint64(fr.Meta.CaptureRefNS))
		bin.PutUint32(rh[24:28], uint32(len(fr.Payload)))
		if _, err := w.Write(rh[:]); err != nil {
			return err
		}
		if _, err := w.Write(fr.Payload); err != nil {
			return err
		}
		sum := sha256.Sum256(fr.Payload)
		if _, err := w.Write(sum[:]); err != nil {
			return err
		}
	}
	return nil
}

// ReadFSEQ parses an FSEQ1 stream, validating magic, record lengths, the
// sample cap and every per-frame checksum. A checksum failure is reported as
// media.ErrChecksumMismatch; a short stream as media.ErrTruncated.
func ReadFSEQ(r io.Reader) ([]*media.RawFrame, error) {
	var hdrBuf [fseqHeaderSize]byte
	if _, err := io.ReadFull(r, hdrBuf[:]); err != nil {
		return nil, media.NewError(media.ErrTruncated, 0, "missing FSEQ header: "+err.Error())
	}
	var magic [5]byte
	copy(magic[:], hdrBuf[0:5])
	if magic != FSEQMagic {
		return nil, media.NewError(media.ErrMetadata, 0, "bad FSEQ magic")
	}
	bin := binary.BigEndian
	hdr := FSEQHeader{
		SourceID:    bin.Uint32(hdrBuf[6:10]),
		Kind:        media.Kind(hdrBuf[10]),
		TimescaleHz: bin.Uint32(hdrBuf[11:15]),
		Width:       bin.Uint32(hdrBuf[15:19]),
		Height:      bin.Uint32(hdrBuf[19:23]),
		FrameCount:  bin.Uint32(hdrBuf[23:27]),
	}
	if hdr.Kind != media.KindVideo && hdr.Kind != media.KindAudio {
		return nil, media.NewError(media.ErrMetadata, hdr.SourceID, "unknown frame kind")
	}
	if uint64(hdr.FrameCount) > media.MaxTotalSamples {
		return nil, media.NewError(media.ErrLimitExceeded, hdr.SourceID, "frame count over limit")
	}
	out := make([]*media.RawFrame, 0, hdr.FrameCount)
	for i := uint32(0); i < hdr.FrameCount; i++ {
		var rh [fseqRecordHdr]byte
		if _, err := io.ReadFull(r, rh[:]); err != nil {
			return nil, media.NewError(media.ErrTruncated, hdr.SourceID, "frame header")
		}
		size := bin.Uint32(rh[24:28])
		if size == 0 || size > media.MaxFrameBytes {
			return nil, media.NewError(media.ErrLengthMismatch, hdr.SourceID, "frame size out of range")
		}
		payload := make([]byte, size)
		if _, err := io.ReadFull(r, payload); err != nil {
			return nil, media.NewError(media.ErrTruncated, hdr.SourceID, "frame payload")
		}
		var stored [32]byte
		if _, err := io.ReadFull(r, stored[:]); err != nil {
			return nil, media.NewError(media.ErrTruncated, hdr.SourceID, "frame checksum")
		}
		if got := sha256.Sum256(payload); got != stored {
			return nil, media.NewError(media.ErrChecksumMismatch, hdr.SourceID, "frame "+utoa32(i))
		}
		out = append(out, &media.RawFrame{
			Kind:    hdr.Kind,
			Payload: payload,
			Meta: media.FrameMeta{
				SourceID:     hdr.SourceID,
				Sequence:     bin.Uint64(rh[0:8]),
				ClockTicks:   bin.Uint64(rh[8:16]),
				CaptureRefNS: int64(bin.Uint64(rh[16:24])),
				TimescaleHz:  uint64(hdr.TimescaleHz),
				Width:        hdr.Width,
				Height:       hdr.Height,
				Size:         size,
				SHA256:       stored,
			},
		})
	}
	// Trailing bytes indicate a framing error rather than silent acceptance.
	if n, err := r.Read(make([]byte, 1)); err == nil && n > 0 {
		return nil, media.NewError(media.ErrLengthMismatch, hdr.SourceID, "unexpected trailing bytes")
	} else if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return out, nil
}

func utoa32(u uint32) string {
	if u == 0 {
		return "0"
	}
	var b [12]byte
	i := len(b)
	for u > 0 {
		i--
		b[i] = byte('0' + u%10)
		u /= 10
	}
	return string(b[i:])
}
