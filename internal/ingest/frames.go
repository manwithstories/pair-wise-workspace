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
	"math"

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

// Resume is the persisted position from which a scan can continue. It mirrors
// the mapper and sequencer snapshots so a restarted run maps the suffix exactly
// as an uninterrupted one would.
type Resume struct {
	Offset    int64
	NextFrame uint64 // frames already delivered before Offset
	Mapper    clock.State
	Sequencer clock.SeqState
}

// Checkpoint is written to a callback whenever a sink boundary is durable.
// Offset points at the next unread box; NextFrame counts frames already
// delivered. Its state is self-contained, so no earlier frame must be reread.
type Checkpoint struct {
	Resume
	LastPTS int64
}

// ScanResume behaves like Scan but starts at res.Position.Offset with the
// mapper and sequencer state restored. Frames already delivered are not
// re-emitted, and the returned Summary counts only the resumed suffix; callers
// combine it with persisted totals. A nil res means a fresh scan.
//
// onDurable, when non-nil, is invoked after each frame the sink accepted, so
// the pipeline can persist a checkpoint whose offset is exactly the next box.
func ScanResume(r io.ReadSeeker, res *Resume, sink FrameSink, onDurable func(Checkpoint) error) (*Summary, error) {
	cp := Checkpoint{}
	if res != nil {
		cp.Resume = *res
	}
	return scan(r, sink, &cp, onDurable)
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
	return scan(r, sink, &Checkpoint{}, nil)
}

func scan(r io.ReadSeeker, sink FrameSink, cp *Checkpoint, onDurable func(Checkpoint) error) (*Summary, error) {
	sum := &Summary{}
	end, err := r.Seek(0, io.SeekEnd)
	if err != nil {
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
	// A resumed scan skips every box before the persisted offset. The track
	// declaration is still parsed so the same timebase is reconstructed and
	// verified against the persisted mapper state.
	startOffset := int64(0)
	if cp.Resume.NextFrame > 0 || cp.Resume.Offset > 0 {
		startOffset = cp.Resume.Offset
		if startOffset < 0 || startOffset > end {
			return nil, model.Fatal("ingest.scan", fmt.Errorf("resume offset %d outside file [0,%d]", startOffset, end))
		}
	}
	// Locate the track declaration from the head regardless of resume point.
	trackSeek := int64(0)
	trackOffset := int64(-1)
	for trackSeek < end {
		tbi, _, err := PeekHeader(r, trackSeek, end)
		if err != nil || tbi == nil {
			break
		}
		if tbi.Type == TypeTrack {
			tb, err := ReadTrackAt(r, trackSeek, tbi.Size)
			if err != nil {
				return nil, model.Fatal("ingest.scan", err)
			}
			track, err := trackFromBox(tb, trackSeek, tbi.Size)
			if err != nil {
				return nil, model.Fatal("ingest.scan", err)
			}
			mapper, err = clock.NewMapper(track.Timebase())
			if err != nil {
				return nil, model.Fatal("ingest.scan", err)
			}
			if err := mapper.SetEpochUnits(int64(track.EpochUnits)); err != nil {
				return nil, model.Fatal("ingest.scan", err)
			}
			if resuming(cp.Resume) {
				if err := mapper.Restore(cp.Resume.Mapper); err != nil {
					return nil, model.Fatal("ingest.scan", err)
				}
				sequencer.Restore(cp.Resume.Sequencer)
			}
			sum.Track = track
			haveTrack = true
			trackOffset = trackSeek
			break
		}
		trackSeek += int64(tbi.Size)
	}
	if !haveTrack {
		return nil, model.Fatal("ingest.scan", errors.New("no track declaration found"))
	}
	offset = startOffset
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
			if offset == trackOffset {
				// Skip the declaration already parsed from the head.
				offset += int64(bi.Size)
				continue
			}
			if truncated {
				return nil, model.Fatal("ingest.scan", fmt.Errorf("%w: track declaration at %d", ErrTruncated, offset))
			}
			return nil, model.Fatal("ingest.scan", errors.New("multiple track declarations"))
		case TypeFrame:
			if truncated {
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
			if onDurable != nil {
				cp.Resume.Offset = offset
				cp.Resume.NextFrame++
				cp.Resume.Mapper = mapper.Snapshot()
				cp.Resume.Sequencer = sequencer.Snapshot()
				cp.LastPTS = fr.PTS
				if err := onDurable(*cp); err != nil {
					return nil, err
				}
			}
			continue
		default:
			offset += int64(bi.Size)
			continue
		}
	}

	// A fresh scan with no frames is fatal. A resumed scan may legitimately
	// find an empty suffix when the interruption landed on the final frame.
	if sum.Frames == 0 && !resuming(cp.Resume) {
		return nil, model.Fatal("ingest.scan", errors.New("no frames found"))
	}
	sum.Repairs = mapper.Repairs()
	return sum, nil
}

func resuming(r Resume) bool { return r.NextFrame > 0 || r.Offset > 0 }

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
//
// targetBytes pads the payload with trailing zero bytes up to a random-access
// frame's nominal size, so the fixture's measured bitrate is realistic. A
// conforming PNG decoder ignores bytes after IEND, so the padded frame still
// validates as a PNG of the declared geometry. A target of zero leaves the
// payload unpadded.
func EncodeFrame(trackID uint32, frameNo uint64, w, h uint32, quality uint8, c model.Color, targetBytes int) ([]byte, error) {
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
	if targetBytes > buf.Len() {
		out := make([]byte, targetBytes)
		copy(out, buf.Bytes())
		// Pattern the padding deterministically so a frame's content is unique
		// to its number and the tail is not a run of identical zero blocks.
		for i := buf.Len(); i < len(out); i++ {
			out[i] = byte((i*31 + int(frameNo)*7 + int(trackID)) % 251)
		}
		return out, nil
	}
	return buf.Bytes(), nil
}

// PayloadDigest is the SHA-256 a frame declares over its payload.
func PayloadDigest(payload []byte) [32]byte { return sha256.Sum256(payload) }

// Cursor is a pull-based scanner. The caller asks for one frame at a time, so
// every downstream stage can stream without a callback and without buffering.
// It exposes the track declaration as soon as it is parsed, which lets the
// manifest alignment recover the timebase from the same pass.
type Cursor struct {
	r       io.ReadSeeker
	end     int64
	track   *Track
	mapper  *clock.Mapper
	seq     clock.Sequencer
	offset  int64
	cur     model.Frame
	done    bool
	started bool
	skipEnd int64 // offset at which a resumed cursor begins
}

// NewCursor locates the track declaration and positions the cursor at the
// first frame (or at the resume offset).
func NewCursor(r io.ReadSeeker, resume *Resume) (*Cursor, error) {
	end, err := r.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, model.Fatal("ingest.cursor", err)
	}
	if end < HeaderSize {
		return nil, model.Fatal("ingest.cursor", errors.New("file too short to contain a header"))
	}
	c := &Cursor{r: r, end: end}
	// Locate the track declaration from the head.
	for off := int64(0); off < end; {
		bi, _, err := PeekHeader(r, off, end)
		if err != nil {
			return nil, model.Fatal("ingest.cursor", err)
		}
		if bi == nil {
			break
		}
		if bi.Type == TypeTrack {
			tb, err := ReadTrackAt(r, off, bi.Size)
			if err != nil {
				return nil, model.Fatal("ingest.cursor", err)
			}
			track, err := trackFromBox(tb, off, bi.Size)
			if err != nil {
				return nil, err
			}
			m, err := clock.NewMapper(track.Timebase())
			if err != nil {
				return nil, model.Fatal("ingest.cursor", err)
			}
			if err := m.SetEpochUnits(int64(track.EpochUnits)); err != nil {
				return nil, model.Fatal("ingest.cursor", err)
			}
			if resume != nil {
				if err := m.Restore(resume.Mapper); err != nil {
					return nil, model.Fatal("ingest.cursor", err)
				}
				c.seq.Restore(resume.Sequencer)
				c.offset = resume.Offset
			}
			c.track, c.mapper = track, m
			break
		}
		off += int64(bi.Size)
	}
	if c.track == nil {
		return nil, model.Fatal("ingest.cursor", errors.New("no track declaration found"))
	}
	if resume != nil {
		c.offset = resume.Offset
	} else {
		// Fresh scan: begin at the first frame after the declaration.
		c.offset = c.track.HeaderOffset + int64(c.track.HeaderSize)
	}
	return c, nil
}

// Track returns the parsed declaration.
func (c *Cursor) Track() *Track {
	if c == nil {
		return nil
	}
	return c.track
}

// Next advances to the next frame. It returns io.EOF at a clean end, and sets
// Done on a torn tail after delivering that final partial frame.
func (c *Cursor) Next() (model.Frame, error) {
	for c.offset < c.end {
		bi, truncated, err := PeekHeader(c.r, c.offset, c.end)
		if err != nil {
			return model.Frame{}, model.Fatal("ingest.cursor", err)
		}
		if bi == nil {
			c.done = true
			return model.Frame{}, io.EOF
		}
		switch bi.Type {
		case TypeFrame:
			if truncated {
				fr, perr := inspectTruncated(c.r, bi, c.offset, c.end, c.track, c.mapper, &c.seq)
				c.done = true
				if perr != nil {
					return model.Frame{}, perr
				}
				if fr == nil {
					return model.Frame{}, io.EOF
				}
				c.offset = c.end
				return *fr, nil
			}
			fr, err := inspectFrame(c.r, bi, c.offset, c.track, c.mapper, &c.seq)
			if err != nil {
				return model.Frame{}, err
			}
			c.offset += int64(bi.Size)
			return fr, nil
		default:
			c.offset += int64(bi.Size)
		}
	}
	c.done = true
	return model.Frame{}, io.EOF
}

// Offset reports the byte offset of the next unread box, the value a
// checkpoint stores so a restart resumes exactly there.
func (c *Cursor) Offset() int64 { return c.offset }

// Snapshot returns the mapper and sequencer state at the current position.
func (c *Cursor) Snapshot() (clock.State, clock.SeqState) {
	return c.mapper.Snapshot(), c.seq.Snapshot()
}

// Repairs returns the accumulated clock repairs.
func (c *Cursor) Repairs() clock.Repairs { return c.mapper.Repairs() }

// ReadTrackDeclaration parses only the track declaration at the head of a
// source, without scanning frames.
func ReadTrackDeclaration(r io.ReadSeeker) (*Track, error) {
	end, err := r.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, err
	}
	for off := int64(0); off < end; {
		bi, _, err := PeekHeader(r, off, end)
		if err != nil {
			return nil, err
		}
		if bi == nil {
			break
		}
		if bi.Type == TypeTrack {
			tb, err := ReadTrackAt(r, off, bi.Size)
			if err != nil {
				return nil, err
			}
			return trackFromBox(tb, off, bi.Size)
		}
		off += int64(bi.Size)
	}
	return nil, errors.New("ingest: no track declaration")
}

// QualityScore decodes a frame payload and returns a deterministic score in
// [0,1]. The score combines luminance contrast with the fraction of pixels
// that differ from their left neighbour, so a flat or heavily clipped image
// scores low and a detailed one scores high. It reads the payload from the
// written frame, keeping the metric honest.
func QualityScore(payload []byte, w, h uint32) (float64, error) {
	img, err := png.Decode(bytes.NewReader(payload))
	if err != nil {
		return 0, err
	}
	b := img.Bounds()
	if b.Dx() == 0 || b.Dy() == 0 {
		return 0, errors.New("ingest: empty image")
	}
	var sum, sumSq float64
	var edge int
	var n float64
	for y := b.Min.Y; y < b.Max.Y; y++ {
		var prevLuma float64 = -1
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, bl, _ := img.At(x, y).RGBA()
			luma := (0.2126*float64(r) + 0.7152*float64(g) + 0.0722*float64(bl)) / 65535
			sum += luma
			sumSq += luma * luma
			n++
			if prevLuma >= 0 && luma != prevLuma {
				edge++
			}
			prevLuma = luma
		}
	}
	mean := sum / n
	variance := sumSq/n - mean*mean
	if variance < 0 {
		variance = 0
	}
	contrast := math.Sqrt(variance) / 0.5 // normalise: std 0.5 is maximum detail
	if contrast > 1 {
		contrast = 1
	}
	detail := float64(edge) / n
	score := 0.5*contrast + 0.5*detail
	if score > 1 {
		score = 1
	}
	return score, nil
}
