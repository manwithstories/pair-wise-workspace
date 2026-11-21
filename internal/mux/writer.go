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

func (o *outBuf) identityMatrix() {
	for _, v := range []int32{0x10000, 0, 0, 0, 0x10000, 0, 0, 0, 0x40000000} {
		o.u32(uint32(v))
	}
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

func planOutput(tracks []*media.Track, plan map[string]uint32) ([]*outTrack, int64, error) {
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
					o.identityMatrix()
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
								o.b = append(o.b, t.entry...)
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
