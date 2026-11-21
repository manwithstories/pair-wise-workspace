package mux

import (
	"encoding/binary"
	"fmt"
	"os"

	"mediacore/internal/media"
)

// outBuf is a minimal big-endian builder for output container boxes.
type outBuf struct{ b []byte }

func (o *outBuf) u8(v uint8)   { o.b = append(o.b, v) }
func (o *outBuf) u16(v uint16) { o.b = binary.BigEndian.AppendUint16(o.b, v) }
func (o *outBuf) u32(v uint32) { o.b = binary.BigEndian.AppendUint32(o.b, v) }
func (o *outBuf) u64(v uint64) { o.b = binary.BigEndian.AppendUint64(o.b, v) }
func (o *outBuf) tag(s string) { o.b = append(o.b, s...) }
func (o *outBuf) zeros(n int)  { o.b = append(o.b, make([]byte, n)...) }

func (o *outBuf) box(typ string, body func()) {
	start := len(o.b)
	o.u32(0)
	o.tag(typ)
	if body != nil {
		body()
	}
	binary.BigEndian.PutUint32(o.b[start:], uint32(len(o.b)-start))
}

func (o *outBuf) full(version uint8, flags uint32) {
	o.u8(version)
	o.u8(uint8(flags >> 16))
	o.u8(uint8(flags >> 8))
	o.u8(uint8(flags))
}

func (o *outBuf) identityMatrix() { o.matrix(0) }

// matrix writes a quarter-turn display matrix.
func (o *outBuf) matrix(rotation int) {
	vals := [9]int32{0x10000, 0, 0, 0, 0x10000, 0, 0, 0, 0x40000000}
	switch ((rotation % 4) + 4) % 4 {
	case 1:
		vals[0], vals[1], vals[3], vals[4] = 0, 0x10000, -0x10000, 0
	case 2:
		vals[0], vals[4] = -0x10000, -0x10000
	case 3:
		vals[0], vals[1], vals[3], vals[4] = 0, -0x10000, 0x10000, 0
	}
	for _, v := range vals {
		o.u32(uint32(v))
	}
}

// decorateEntry returns the copied input sample entry with the resolved
// colour and crop appended as child boxes. The original entry's bytes are
// preserved; the resolved description is added, never substituted.
func decorateEntry(t *outTrack) []byte {
	entry := append([]byte(nil), t.entry...)
	if len(entry) < 8 {
		return entry
	}
	var extra []byte
	if t.color.Present || t.color.Source == "default" {
		b := &outBuf{}
		b.box("colr", func() {
			b.tag("nclx")
			b.u16(t.color.Primaries)
			b.u16(t.color.Transfer)
			b.u16(t.color.Matrix)
			if t.color.FullRange {
				b.u8(0x80)
			} else {
				b.u8(0)
			}
		})
		extra = append(extra, b.b...)
	}
	// Video sample entries always carry a clap when the resolved aperture is
	// not the full frame, even if the coded dimensions were not available.
	if t.crop != nil && t.crop.Width > 0 && t.crop.Height > 0 &&
		(t.crop.X != 0 || t.crop.Y != 0 ||
			t.crop.Width != t.width || t.crop.Height != t.height) {
		b := &outBuf{}
		// clap is four signed 16.16 rationals: width, height, horizontal
		// offset and vertical offset, each with a denominator.
		b.box("clap", func() {
			hoff, voff := 0, 0
			if t.width > 0 && t.height > 0 {
				hoff = t.crop.X - (t.width-t.crop.Width)/2
				voff = t.crop.Y - (t.height-t.crop.Height)/2
			}
			// Each value is 16.16 fixed point; denominators are 1.
			for _, v := range []int{t.crop.Width << 16, 1 << 16, t.crop.Height << 16, 1 << 16,
				hoff << 16, 1 << 16, voff << 16, 1 << 16} {
				b.u32(uint32(int32(v)))
			}
		})
		extra = append(extra, b.b...)
	}
	if len(extra) == 0 {
		return entry
	}
	// The sample entry is itself the outer box, so growth must be reflected
	// in its own size field. Building a fresh slice also keeps the caller's
	// entry untouched.
	out := make([]byte, 0, len(entry)+len(extra))
	out = append(out, entry...)
	out = append(out, extra...)
	binary.BigEndian.PutUint32(out[0:], uint32(len(out)))
	return out
}

// run is a run-length encoded stts/ctts entry.
type run struct {
	count uint32
	value int64
}

// outTrack is the writer's per-track state, derived from the unified model
// and never from decoded essence.
type outTrack struct {
	id        uint32
	key       string
	timescale uint32
	video     bool
	width     int
	height    int
	entry     []byte
	sizes     []uint32
	offsets   []int64
	stts      []run
	ctts      []run
	hasCTTS   bool
	// rotation is the resolved counter-clockwise quarter-turn display
	// rotation, written into the output display matrix.
	rotation int
	// color is the resolved colour description, written as an nclx colr box.
	color media.Color
	// crop is the resolved clean aperture, written as a clap box when it is
	// not the full frame.
	crop *media.Crop
}

func (t *outTrack) duration() uint64 {
	if t.timescale == 0 {
		return 0
	}
	var ticks int64
	for _, r := range t.stts {
		ticks += int64(r.count) * r.value
	}
	return uint64(ticks)
}

// ResolvedMeta is the resolved geometry and colour for one track. It is the
// only source of those values in the output, so container/track/override
// priority is applied exactly once.
type ResolvedMeta struct {
	Rotation int
	Crop     *media.Crop
	Color    media.Color
}

func planOutput(tracks []*media.Track, plan map[string]uint32, resolved map[string]ResolvedMeta) ([]*outTrack, int64, error) {
	out := make([]*outTrack, 0, len(tracks))
	for _, t := range tracks {
		key := t.ID.String()
		ot := &outTrack{
			id:        plan[key],
			key:       key,
			timescale: t.Timescale,
			video:     t.Type == media.MediaVideo,
			width:     t.Width,
			height:    t.Height,
			entry:     t.SampleEntry,
		}
		if ot.timescale == 0 {
			ot.timescale = 1000
		}
		if r, ok := resolved[key]; ok {
			ot.rotation = r.Rotation
			ot.crop = r.Crop
			ot.color = r.Color
		}
		if len(ot.entry) == 0 {
			ot.entry = fallbackEntry(t)
		}
		n := t.SampleCount()
		var prevDelta, prevOff int64
		for i := 0; i < n; i++ {
			s, err := t.Index.At(i)
			if err != nil {
				return nil, 0, media.WithTrackContext(err, t.ID.Container, t.ID.Value)
			}
			if s.Size <= 0 || s.Size > int64(^uint32(0)) {
				return nil, 0, media.NewError(media.ClassInvalidLength,
					"sample %d of track %s has an unsupported size %d", i, key, s.Size).
					WithContainer(t.ID.Container).WithTrack(t.ID.Value)
			}
			ot.sizes = append(ot.sizes, uint32(s.Size))
			delta := media.RatToTicks(s.Duration, ot.timescale)
			if delta < 0 {
				return nil, 0, media.NewError(media.ClassMetadataConflict,
					"sample %d of track %s has a negative duration", i, key).
					WithContainer(t.ID.Container).WithTrack(t.ID.Value)
			}
			ot.stts = appendRun(ot.stts, delta)
			off := media.RatToTicks(s.PTS.Sub(s.DTS), ot.timescale)
			if off != 0 {
				ot.hasCTTS = true
			}
			ot.ctts = appendRun(ot.ctts, off)
			prevDelta, prevOff = delta, off
		}
		// Silence the unused-variable guard while keeping the intent clear:
		// the last values are not special-cased.
		_, _ = prevDelta, prevOff
		out = append(out, ot)
	}
	return out, layoutStart(out), nil
}

func appendRun(runs []run, v int64) []run {
	if n := len(runs); n > 0 && runs[n-1].value == v {
		runs[n-1].count++
		return runs
	}
	return append(runs, run{count: 1, value: v})
}

// layoutStart computes the first payload byte and fills sample offsets,
// switching to co64 if the offsets do not fit in 32 bits. The offset arrays
// are sized before the header is measured, because the header's length
// depends on the number of chunk offsets it carries.
func layoutStart(tracks []*outTrack) int64 {
	for _, t := range tracks {
		t.offsets = make([]int64, len(t.sizes))
	}
	header := buildHeader(tracks, false)
	start := int64(len(header))
	var pos = start
	for _, t := range tracks {
		for i, s := range t.sizes {
			t.offsets[i] = pos
			pos += int64(s)
		}
	}
	for _, t := range tracks {
		for _, off := range t.offsets {
			if off > int64(^uint32(0)) {
				co64 := buildHeader(tracks, true)
				start = int64(len(co64))
				pos = start
				for _, t2 := range tracks {
					for i, s := range t2.sizes {
						t2.offsets[i] = pos
						pos += int64(s)
					}
				}
				return start
			}
		}
	}
	return start
}

// buildHeader serializes ftyp and moov, then an mdat header. The payload
// length is implied by the sample tables.
func buildHeader(tracks []*outTrack, co64 bool) []byte {
	o := &outBuf{}
	o.box("ftyp", func() {
		o.tag("isom")
		o.u32(0x200)
		o.tag("isom")
		o.tag("iso2")
		o.tag("mp41")
	})
	o.box("moov", func() {
		o.box("mvhd", func() {
			o.full(0, 0)
			o.u32(0)
			o.u32(0)
			o.u32(1000)
			o.u32(0)
			o.u32(0x00010000)
			o.u16(0x0100)
			o.u16(0)
			o.zeros(8)
			o.identityMatrix()
			o.zeros(24)
			o.u32(uint32(len(tracks) + 1))
		})
		for _, t := range tracks {
			t := t
			o.box("trak", func() {
				o.box("tkhd", func() {
					o.full(0, 3)
					o.u32(0)
					o.u32(0)
					o.u32(t.id)
					o.u32(0)
					o.u32(uint32(t.duration() * 1000 / uint64(t.timescale)))
					o.zeros(8)
					o.u16(0)
					o.u16(0)
					if t.video {
						o.u16(0)
					} else {
						o.u16(0x0100)
					}
					o.u16(0)
					o.matrix(t.rotation)
					o.u32(uint32(t.width) << 16)
					o.u32(uint32(t.height) << 16)
				})
				o.box("mdia", func() {
					o.box("mdhd", func() {
						o.full(0, 0)
						o.u32(0)
						o.u32(0)
						o.u32(t.timescale)
						o.u32(uint32(t.duration()))
						o.u16(0x55c4)
						o.u16(0)
					})
					o.box("hdlr", func() {
						o.full(0, 0)
						o.u32(0)
						if t.video {
							o.tag("vide")
						} else {
							o.tag("soun")
						}
						o.zeros(12)
						o.u8(0)
					})
					o.box("minf", func() {
						o.box("stbl", func() {
							o.box("stsd", func() {
								o.full(0, 0)
								o.u32(1)
								o.b = append(o.b, decorateEntry(t)...)
							})
							o.box("stts", func() {
								o.full(0, 0)
								o.u32(uint32(len(t.stts)))
								for _, r := range t.stts {
									o.u32(r.count)
									o.u32(uint32(r.value))
								}
							})
							if t.hasCTTS {
								o.box("ctts", func() {
									o.full(1, 0)
									o.u32(uint32(len(t.ctts)))
									for _, r := range t.ctts {
										o.u32(r.count)
										o.u32(uint32(int32(r.value)))
									}
								})
							}
							o.box("stsc", func() {
								o.full(0, 0)
								o.u32(1)
								o.u32(1)
								o.u32(1)
								o.u32(1)
							})
							o.box("stsz", func() {
								o.full(0, 0)
								o.u32(0)
								o.u32(uint32(len(t.sizes)))
								for _, s := range t.sizes {
									o.u32(s)
								}
							})
							if co64 {
								o.box("co64", func() {
									o.full(0, 0)
									o.u32(uint32(len(t.offsets)))
									for _, off := range t.offsets {
										o.u64(uint64(off))
									}
								})
							} else {
								o.box("stco", func() {
									o.full(0, 0)
									o.u32(uint32(len(t.offsets)))
									for _, off := range t.offsets {
										o.u32(uint32(off))
									}
								})
							}
						})
					})
				})
			})
		}
	})
	var payload int64
	for _, t := range tracks {
		for _, s := range t.sizes {
			payload += int64(s)
		}
	}
	o.u32(uint32(8 + payload))
	o.tag("mdat")
	return o.b
}

// writeMP4Header writes the structural header. The caller must have sized
// the file consistently with layoutStart.
func writeMP4Header(f *os.File, tracks []*outTrack, start int64) error {
	co64 := false
	for _, t := range tracks {
		for _, off := range t.offsets {
			if off > int64(^uint32(0)) {
				co64 = true
			}
		}
	}
	header := buildHeader(tracks, co64)
	if int64(len(header)) != start {
		return fmt.Errorf("internal layout mismatch: header is %d bytes, payload planned at %d",
			len(header), start)
	}
	if _, err := f.WriteAt(header, 0); err != nil {
		return media.NewError(media.ClassIO, "cannot write output header").WithCause(err)
	}
	return nil
}

// fallbackEntry is used when an adapter cannot supply the input sample
// entry. The fourcc follows the track's codec where one is known; the output
// remains parseable but codec fidelity is the adapter's responsibility.
func fallbackEntry(t *media.Track) []byte {
	o := &outBuf{}
	if t.Type == media.MediaVideo {
		fourcc := "avc1"
		switch t.Codec {
		case "hvc1", "hev1":
			fourcc = "hvc1"
		case "mxf-j2k":
			fourcc = "mjp2"
		case "av01":
			fourcc = "av01"
		}
		o.box(fourcc, func() {
			o.zeros(6)
			o.u16(1)
			o.u16(0)
			o.u16(0)
			o.zeros(12)
			o.u16(uint16(t.Width))
			o.u16(uint16(t.Height))
			o.u32(0x00480000)
			o.u32(0x00480000)
			o.u32(0)
			o.u16(1)
			o.zeros(32)
			o.u16(0x0018)
			o.u16(0xffff)
		})
	} else {
		fourcc := "mp4a"
		if t.Codec == "pcm_s24le" {
			fourcc = "lpcm"
		}
		o.box(fourcc, func() {
			o.zeros(6)
			o.u16(1)
			o.u16(0)
			o.u16(0)
			o.u32(0)
			o.u16(2)
			o.u16(16)
			o.u16(0)
			o.u16(0)
			o.u32(48000 << 16)
		})
	}
	return o.b
}
