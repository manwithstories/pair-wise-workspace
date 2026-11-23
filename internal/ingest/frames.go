package ingest

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"

	"github.com/abema/go-mp4"

	"mediad/internal/clock"
	"mediad/internal/model"
)

// Track is the parsed declaration for one source file.
type Track struct {
	ID         uint32
	Width      uint32
	Height     uint32
	CaptureUPS uint32
	Timescale  uint32
	EpochUnits uint64
	WrapUnits  int64
	TargetKbps uint32
	Color      model.Color
	FrameCount uint32
	// HeaderOffset/HeaderSize locate the declaration box for re-reading.
	HeaderOffset int64
	HeaderSize   uint64
}

// Timebase returns the clock descriptor for this track.
func (t *Track) Timebase() clock.Timebase {
	return clock.Timebase{Timescale: t.Timescale, CaptureUPS: t.CaptureUPS, WrapUnits: t.WrapUnits}
}

// FrameSink receives each parsed frame. Returning an error stops the scan and
// is propagated unchanged, which lets the pipeline abort on interruption.
type FrameSink func(model.Frame) error

// Summary aggregates what a scan learned. Counters are O(1) and keep no
// reference to individual frames, so scanning memory is independent of the
// file's frame count.
type Summary struct {
	Track     *Track
	Frames    uint64 // complete frames observed, including bad ones
	HashBad   uint64
	ImageBad  uint64
	Truncated uint64
	Conflicts uint64
	Dup       uint64
	LastEnd   int64 // offset just past the last complete frame
	LastPTS   int64
	Repairs   clock.Repairs
}

// Scan reads a container sequentially from start to end, decoding the track
// declaration and every frame, and delivers frames to sink in source order.
//
// Damage isolation: a frame whose declared digest does not match, or whose
// payload is not the declared image, is reported with a non-OK status and the
// scan continues. Only a missing or unreadable track declaration, or a file
// with no decodable frames, is fatal. A frame whose box extends past EOF is a
// truncated tail: it is reported once and ends the scan.
func Scan(r io.ReadSeeker, sink FrameSink) (*Summary, error) {
	sum := &Summary{}
	end, err := r.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, model.Fatal("ingest.scan", err)
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return nil, model.Fatal("ingest.scan", err)
	}

	var (
		mapper    *clock.Mapper
		sequencer clock.Sequencer
		haveTrack bool
		offset    int64
	)
	if end < HeaderSize {
		return nil, model.Fatal("ingest.scan", errors.New("file too short to contain a header"))
	}

	for offset < end {
		bi, truncated, err := PeekHeader(r, offset, end)
		if err != nil {
			return nil, model.Fatal("ingest.scan", fmt.Errorf("header at %d: %w", offset, err))
		}
		if bi == nil {
			// Fewer than 8 bytes remain: a torn trailing header.
			sum.Truncated = 1
			break
		}
		switch bi.Type {
		case mp4.BoxTypeFtyp():
			offset += int64(bi.Size)
			continue
		case TypeTrack:
			if truncated {
				return nil, model.Fatal("ingest.scan", fmt.Errorf("%w: track declaration at %d", ErrTruncated, offset))
			}
			tb, err := ReadTrackAt(r, offset, bi.Size)
			if err != nil {
				return nil, model.Fatal("ingest.scan", fmt.Errorf("track declaration at %d: %w", offset, err))
			}
			track, err := trackFromBox(tb, offset, bi.Size)
			if err != nil {
				return nil, model.Fatal("ingest.scan", err)
			}
			if haveTrack {
				return nil, model.Fatal("ingest.scan", errors.New("multiple track declarations"))
			}
			mapper, err = clock.NewMapper(track.Timebase())
			if err != nil {
				return nil, model.Fatal("ingest.scan", err)
			}
			if err := mapper.SetEpochUnits(int64(track.EpochUnits)); err != nil {
				return nil, model.Fatal("ingest.scan", err)
			}
			sum.Track = track
			haveTrack = true
			offset += int64(bi.Size)
			continue
		case TypeFrame:
			if !haveTrack {
				return nil, model.Fatal("ingest.scan", errors.New("frame before track declaration"))
			}
			if truncated {
				// The source ended inside this box. The sink still receives a
				// truncated record when the header was complete enough to
				// describe it, so the report can name the frame.
				fr, perr := inspectTruncated(r, bi, offset, end, sum.Track, mapper, &sequencer)
				if perr != nil {
					return nil, perr
				}
				if fr != nil {
					if err := sink(*fr); err != nil {
						return nil, err
					}
					sum.Frames++
					sum.Truncated++
				}
				sum.LastEnd = end
				return sum, nil
			}
			fr, err := inspectFrame(r, bi, offset, sum.Track, mapper, &sequencer)
			if err != nil {
				return nil, err
			}
			if err := sink(fr); err != nil {
				return nil, err
			}
			sum.Frames++
			switch fr.Status {
			case model.FrameHashMismatch:
				sum.HashBad++
			case model.FrameImageInvalid:
				sum.ImageBad++
			case model.FrameConflict:
				sum.Conflicts++
			case model.FrameDuplicate:
				sum.Dup++
			}
			sum.LastPTS = fr.PTS
			offset += int64(bi.Size)
			sum.LastEnd = offset
			continue
		default:
			offset += int64(bi.Size)
			continue
		}
	}

	if !haveTrack {
		return nil, model.Fatal("ingest.scan", errors.New("no track declaration found"))
	}
	if sum.Frames == 0 {
		return nil, model.Fatal("ingest.scan", errors.New("no frames found"))
	}
	sum.Repairs = mapper.Repairs()
	return sum, nil
}

func trackFromBox(tb *TrackBox, offset int64, size uint64) (*Track, error) {
	t := &Track{
		ID:           tb.TrackID,
		Width:        tb.Width,
		Height:       tb.Height,
		CaptureUPS:   tb.CaptureUPS,
		Timescale:    tb.Timescale,
		EpochUnits:   tb.EpochUnits,
		WrapUnits:    int64(tb.WrapUnits),
		TargetKbps:   tb.TargetKbps,
		Color:        model.Color{Primaries: tb.Color[0], Transfer: tb.Color[1], Matrix: tb.Color[2], Range: tb.Color[3]},
		FrameCount:   tb.FrameCount,
		HeaderOffset: offset,
		HeaderSize:   size,
	}
	if err := t.Timebase().Validate(); err != nil {
		return nil, model.Fatal("ingest.track", err)
	}
	return t, nil
}

func frameMetaFromBox(fb *FrameBox, t *Track) model.FrameMeta {
	return model.FrameMeta{
		TrackID:      fb.TrackID,
		FrameNo:      fb.FrameNo,
		CaptureUnits: fb.CaptureUnits,
		Width:        fb.Width,
		Height:       fb.Height,
		CaptureUPS:   fb.CaptureUPS,
		Timescale:    fb.Timescale,
		Color:        model.Color{Primaries: fb.Color[0], Transfer: fb.Color[1], Matrix: fb.Color[2], Range: fb.Color[3]},
	}
}

// inspectFrame validates one complete frame and classifies it. It never
// returns an error for a bad frame: the defect is carried in Frame.Status.
func inspectFrame(r io.ReadSeeker, bi *mp4.BoxInfo, offset int64, t *Track, mapper *clock.Mapper, seq *clock.Sequencer) (model.Frame, error) {
	fb, err := ReadFrameAt(r, offset, bi.Size)
	if err != nil {
		// The box was fully present on disk but its payload would not decode.
		// Treat it as an isolated bad frame, not a file-level failure.
		return model.Frame{
			Meta:   model.FrameMeta{TrackID: t.ID},
			Offset: offset, Size: uint32(bi.Size), Status: model.FrameImageInvalid,
			Detail: "payload decode: " + err.Error(),
		}, nil
	}
	meta := frameMetaFromBox(fb, t)
	digest := model.DigestBytes(fb.Payload)
	fr := model.Frame{Meta: meta, Offset: offset, Size: uint32(bi.Size), Digest: digest, DeclPTS: fb.PTS}

	pts, _, err := mapper.Map(int64(fb.CaptureUnits))
	if err != nil {
		return fr, model.Recoverable("ingest.map", int(t.ID), fb.FrameNo, "clock", err)
	}
	fr.PTS = pts
	fr.Meta.Timescale = t.Timescale
	fr.Meta.CaptureUPS = t.CaptureUPS

	switch {
	case uint32(len(fb.Payload)) != fb.PayloadLen:
		fr.Status = model.FrameTruncatedTail
		fr.Detail = fmt.Sprintf("payload %d of declared %d", len(fb.Payload), fb.PayloadLen)
	case digest != fb.DeclaredHash:
		fr.Status = model.FrameHashMismatch
		fr.Detail = fmt.Sprintf("declared %x, actual %x", fb.DeclaredHash[:8], digest[:8])
	case !validImage(fb.Payload, meta.Width, meta.Height):
		fr.Status = model.FrameImageInvalid
		fr.Detail = "payload is not a PNG of the declared size"
	}

	// Sequence checks apply to content that otherwise decoded; a broken frame
	// must not be mistaken for a duplicate of the previous one.
	if fr.Status == model.FrameOK {
		switch seq.Accept(fb.FrameNo, digest) {
		case clock.SeqDuplicate:
			fr.Status = model.FrameDuplicate
			fr.Detail = "identical frame number and content"
		case clock.SeqConflict, clock.SeqOutOfOrder:
			fr.Status = model.FrameConflict
			fr.Detail = "frame number does not advance"
		}
	}
	return fr, nil
}

// inspectTruncated handles a frame box that runs past EOF. The header is
// available, so the frame can be named; its payload length is whatever bytes
// remain.
func inspectTruncated(r io.ReadSeeker, bi *mp4.BoxInfo, offset, end int64, t *Track, mapper *clock.Mapper, seq *clock.Sequencer) (*model.Frame, error) {
	avail := end - offset - HeaderSize
	if avail <= 0 {
		return nil, nil
	}
	if _, err := r.Seek(offset+HeaderSize, io.SeekStart); err != nil {
		return nil, model.Fatal("ingest.truncated", err)
	}
	buf := make([]byte, avail)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, model.Fatal("ingest.truncated", err)
	}
	fb, _, uerr := mp4.UnmarshalAny(bytes.NewReader(buf), TypeFrame, uint64(avail), mp4.Context{})
	if uerr != nil {
		// Header bytes survive but the record is not even decodable; still
		// report a truncated tail so the frame count stays honest.
		return &model.Frame{
			Meta:   model.FrameMeta{TrackID: t.ID, CaptureUnits: 0},
			Offset: offset, Size: uint32(end - offset),
			Status: model.FrameTruncatedTail, Detail: "undecodable tail",
		}, nil
	}
	fbox, ok := fb.(*FrameBox)
	if !ok {
		return nil, nil
	}
	meta := frameMetaFromBox(fbox, t)
	fr := model.Frame{
		Meta: meta, Offset: offset, Size: uint32(end - offset),
		Digest: model.DigestBytes(fbox.Payload),
		Status: model.FrameTruncatedTail,
		Detail: fmt.Sprintf("tail carries %d of declared %d payload bytes", len(fbox.Payload), fbox.PayloadLen),
	}
	if pts, _, err := mapper.Map(int64(fbox.CaptureUnits)); err == nil {
		fr.PTS = pts
	}
	seq.Accept(fbox.FrameNo, fr.Digest)
	return &fr, nil
}

// validImage reports whether payload decodes as a PNG with exactly w×h pixels.
func validImage(payload []byte, w, h uint32) bool {
	cfg, err := png.DecodeConfig(bytes.NewReader(payload))
	if err != nil {
		return false
	}
	if uint32(cfg.Width) != w || uint32(cfg.Height) != h {
		return false
	}
	img, err := png.Decode(bytes.NewReader(payload))
	if err != nil {
		return false
	}
	b := img.Bounds()
	return b.Dx() == int(w) && b.Dy() == int(h)
}

// EncodeFrame builds the deterministic PNG payload used by fixtures. The image
// encodes track, frame number and a quality ramp so that re-running the demo
// yields byte-identical files and therefore identical digests.
func EncodeFrame(trackID uint32, frameNo uint64, w, h uint32, quality uint8, c model.Color) ([]byte, error) {
	img := image.NewRGBA(image.Rect(0, 0, int(w), int(h)))
	for y := 0; y < int(h); y++ {
		for x := 0; x < int(w); x++ {
			// A deterministic pattern mixing coordinates, frame number and the
			// declared quality. Frames differ, so duplicates are detectable.
			v := uint8((uint32(x)*7 + uint32(y)*13 + uint32(frameNo)*31 + uint32(trackID)*3) % 251)
			if int(v) > int(quality) {
				v = quality
			}
			img.SetRGBA(x, y, color.RGBA{R: v, G: c.Primaries, B: c.Transfer, A: 0xff})
		}
	}
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestSpeed}
	if err := enc.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// PayloadDigest is the SHA-256 a frame declares over its payload.
func PayloadDigest(payload []byte) [32]byte { return sha256.Sum256(payload) }
