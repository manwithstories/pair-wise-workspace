// Package render produces deterministic, off-screen frame sequences for
// merged segments. There is no GUI dependency: every frame is a paletted
// raster drawn with the standard library, encoded as PNG and packed into a
// small archive container. Given the same segment, two runs emit byte-for-
// byte identical output, which the report layer hashes and verifies.
package render

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash"
	"image"
	"image/color"
	"image/png"
	"sort"

	"archivetl/internal/clock"
	"archivetl/internal/pipeline"
	"archivetl/internal/timeline"
)

// Container magic and version for the off-screen frame archive.
const (
	Magic   = "AFR1" // archive frame revision 1
	FrameW  = 320
	FrameH  = 180
	headerH = 16
)

// FrameRate of the exported sequence, as archive ticks per frame slot.
// One slot equals 100 ms, so segment windows map onto whole frame counts.
const slotTicks clock.Tick = 3000

// palette assigns each lane a fixed, order-dependent color. Lane count is
// bounded by len(palette); further lanes cycle through the same colors with
// a reduced value channel.
var palette = [7]color.RGBA{
	{0x1f, 0x77, 0xb4, 0xff}, // blue
	{0xff, 0x7f, 0x0e, 0xff}, // orange
	{0x2c, 0xa0, 0x2c, 0xff}, // green
	{0xd6, 0x27, 0x28, 0xff}, // red
	{0x94, 0x67, 0xbd, 0xff}, // purple
	{0x8c, 0x56, 0x4b, 0xff}, // brown
	{0xe3, 0x77, 0xc2, 0xff}, // pink
}

var (
	bgColor     = color.RGBA{0x0f, 0x11, 0x15, 0xff}
	gridColor   = color.RGBA{0x2a, 0x2e, 0x36, 0xff}
	borderColor = color.RGBA{0x55, 0x5b, 0x66, 0xff}
)

// FrameRenderer implements pipeline.Renderer. It also stamps the segment's
// deterministic input hash, which the sink uses for resume decisions.
type FrameRenderer struct {
	// FramesPerSegment caps how many slots are exported; zero uses the
	// natural count derived from the segment's time span.
	FramesPerSegment int
}

// Render builds the frame container and returns its bytes and SHA-256.
func (r FrameRenderer) Render(ctx context.Context, seg *pipeline.Segment) ([]byte, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	seg.InputHash = InputFingerprint(seg)

	slots := r.FramesPerSegment
	if slots <= 0 {
		span := seg.End - seg.Start
		slots = int((span + slotTicks - 1) / slotTicks)
		if slots < 1 {
			slots = 1
		}
	}

	out := &bytes.Buffer{}
	out.WriteString(Magic)
	writeU32(out, uint32(FrameW))
	writeU32(out, uint32(FrameH))
	writeU32(out, uint32(slots))

	events := timeline.Flatten(seg.Lanes)
	laneCount := len(seg.Lanes)
	for frame := 0; frame < slots; frame++ {
		if err := ctx.Err(); err != nil {
			return nil, "", err
		}
		img := drawFrame(seg, events, laneCount, frame, slots)
		var pngBuf bytes.Buffer
		if err := png.Encode(&pngBuf, img); err != nil {
			return nil, "", fmt.Errorf("render frame %d: %w", frame, err)
		}
		writeU32(out, uint32(pngBuf.Len()))
		out.Write(pngBuf.Bytes())
	}
	sum := sha256.Sum256(out.Bytes())
	seg.OutputHash = hex.EncodeToString(sum[:])
	return out.Bytes(), seg.OutputHash, nil
}

func drawFrame(seg *pipeline.Segment, events []timeline.Event,
	laneCount, frame, slots int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, FrameW, FrameH))
	fill(img, bgColor)

	// Header band marks the segment id deterministically via stripe width.
	for x := 0; x < FrameW; x++ {
		if x%32 < (seg.ID%8 + 1) {
			for y := 0; y < 4; y++ {
				img.SetRGBA(x, y, gridColor)
			}
		}
	}
	// Vertical gridlines, one per frame slot.
	for i := 0; i <= slots; i++ {
		x := i * FrameW / slots
		if x < FrameW {
			for y := headerH; y < FrameH; y++ {
				img.SetRGBA(x, y, gridColor)
			}
		}
	}
	// Border.
	for x := 0; x < FrameW; x++ {
		img.SetRGBA(x, FrameH-1, borderColor)
	}

	if laneCount == 0 {
		return img
	}
	laneH := (FrameH - headerH) / laneCount
	slotStart := seg.Start + clock.Tick(frame)*slotTicks
	slotEnd := slotStart + slotTicks

	// Events stay sorted by global order; drawing is therefore deterministic.
	sort.SliceStable(events, func(i, j int) bool { return events[i].Order < events[j].Order })
	for _, ev := range events {
		if ev.End <= slotStart || ev.Start >= slotEnd {
			continue
		}
		lane := ev.Lane % len(palette)
		y0 := headerH + (ev.Lane%laneCount)*laneH
		y1 := y0 + laneH - 2
		x0 := mapX(ev.Start, seg.Start, seg.End)
		x1 := mapX(ev.End, seg.Start, seg.End)
		if x1 <= x0 {
			x1 = x0 + 1 // zero-duration cue still renders as a marker
		}
		col := palette[lane]
		for y := y0; y <= y1 && y < FrameH; y++ {
			for x := x0; x < x1 && x < FrameW; x++ {
				img.SetRGBA(x, y, col)
			}
		}
	}
	return img
}

func mapX(t, start, end clock.Tick) int {
	if end <= start {
		return 0
	}
	rel := int64(t - start)
	span := int64(end - start)
	return int(rel) * FrameW / int(span)
}

func fill(img *image.RGBA, c color.RGBA) {
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			img.SetRGBA(x, y, c)
		}
	}
}

func writeU32(buf *bytes.Buffer, v uint32) {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	buf.Write(b[:])
}

// InputFingerprint hashes the exact ordered records that define a segment.
// Only field values participate, so the digest is stable across processes.
func InputFingerprint(seg *pipeline.Segment) string {
	h := sha256.New()
	writeString := func(s string) {
		var l [4]byte
		binary.BigEndian.PutUint32(l[:], uint32(len(s)))
		h.Write(l[:])
		h.Write([]byte(s))
	}
	writeInt64 := func(v int64) {
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], uint64(v))
		h.Write(b[:])
	}
	writeInt64(int64(seg.ID))
	writeInt64(int64(seg.Start))
	writeInt64(int64(seg.End))
	for _, rec := range seg.Records {
		writeString(rec.Track)
		writeString(rec.Source)
		writeInt64(int64(rec.Sequence))
		writeInt64(rec.Offset)
		writeInt64(int64(rec.Start))
		writeInt64(int64(rec.End))
		writeString(rec.Text)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// HashWriter exposes a streaming hasher for callers building summaries.
func HashWriter() hash.Hash { return sha256.New() }
