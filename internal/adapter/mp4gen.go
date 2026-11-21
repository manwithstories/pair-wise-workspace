package adapter

import (
	"encoding/binary"
	"fmt"
	"os"
)

// MP4Spec describes a deterministic MP4 fixture. Every byte is derived from
// the spec, so regenerating yields identical files.
type MP4Spec struct {
	Tracks       int
	Samples      int
	Seed         int64
	Rotation     int // quarter turns at movie level
	Color        *MediaColor
	Crop         *MediaCrop
	UnknownBoxes int
	CTTS         bool
	// Corrupt selects a structural fault: "", "truncate" or "sample_length".
	Corrupt string
	// Offset is used by "truncate" and by "sample_length" (sample ordinal).
	Offset int64
}

// MediaCrop describes a clean aperture in coded pixels.
type MediaCrop struct{ X, Y, Width, Height int }

// MediaColor describes an nclx colour element.
type MediaColor struct {
	Primaries, Transfer, Matrix uint16
	FullRange                   bool
}

// MP4Layout is the generated prefix plus the derived payload size. The mdat
// payload itself is not materialized; callers either extend a sparse file or
// use LogicalSource to synthesize it.
type MP4Layout struct {
	Prefix      []byte
	PayloadSize int64
	// TruncateAt is non-zero when the spec requests truncation.
	TruncateAt int64
	// SampleOffsets maps [track][sample] to the absolute payload offset, used
	// by tests to check integrity.
	SampleOffsets [][]int64
	SampleSizes   [][]int
}

// TotalSize is the size the completed fixture would have.
func (l *MP4Layout) TotalSize() int64 { return int64(len(l.Prefix)) + l.PayloadSize }

type byteBuf struct {
	b []byte
}

func (b *byteBuf) u8(v uint8)      { b.b = append(b.b, v) }
func (b *byteBuf) u16(v uint16)    { b.b = binary.BigEndian.AppendUint16(b.b, v) }
func (b *byteBuf) u32(v uint32)    { b.b = binary.BigEndian.AppendUint32(b.b, v) }
func (b *byteBuf) u64(v uint64)    { b.b = binary.BigEndian.AppendUint64(b.b, v) }
func (b *byteBuf) zeros(n int)     { b.b = append(b.b, make([]byte, n)...) }
func (b *byteBuf) fourcc(s string) { b.b = append(b.b, s...) }

func (b *byteBuf) fullbox(v uint8, flags uint32) {
	b.u8(v)
	b.u8(uint8(flags >> 16))
	b.u8(uint8(flags >> 8))
	b.u8(uint8(flags))
}

// box appends a box header, runs body, then patches the size.
func (b *byteBuf) box(typ string, body func()) {
	start := len(b.b)
	b.u32(0)
	b.fourcc(typ)
	if body != nil {
		body()
	}
	binary.BigEndian.PutUint32(b.b[start:], uint32(len(b.b)-start))
}

func (b *byteBuf) matrix(rotation int) {
	vals := [9]int32{0x10000, 0, 0, 0, 0x10000, 0, 0, 0, 0x40000000}
	switch rotation {
	case 1:
		vals[0], vals[1], vals[3], vals[4] = 0, 0x10000, -0x10000, 0
	case 2:
		vals[0], vals[4] = -0x10000, -0x10000
	case 3:
		vals[0], vals[1], vals[3], vals[4] = 0, -0x10000, 0x10000, 0
	}
	for _, v := range vals {
		b.u32(uint32(v))
	}
}

// sampleSize is deterministic and differs between video and audio.
func sampleSize(video bool, i int) int {
	if video {
		return 900 + (i*37)%211
	}
	return 320 + (i*13)%47
}

// BuildMP4 constructs the complete structural prefix of the fixture.
func BuildMP4(spec MP4Spec) (*MP4Layout, error) {
	if spec.Tracks < 1 || spec.Tracks > 8 {
		return nil, fmt.Errorf("tracks must be in 1..8")
	}
	if spec.Samples < 1 {
		return nil, fmt.Errorf("samples must be positive")
	}
	if spec.Rotation < 0 || spec.Rotation > 3 {
		return nil, fmt.Errorf("rotation must be 0..3")
	}
	if spec.Corrupt == "truncate" && spec.Offset <= 0 {
		return nil, fmt.Errorf("truncate requires a positive offset")
	}
	if spec.Corrupt == "sample_length" && (spec.Offset < 0 || spec.Offset >= int64(spec.Samples)) {
		return nil, fmt.Errorf("sample_length offset must name a sample")
	}
	if spec.UnknownBoxes < 0 {
		return nil, fmt.Errorf("unknown box count must not be negative")
	}

	sizes := make([][]int, spec.Tracks)
	var payload int64
	offsets := make([][]int64, spec.Tracks)
	for t := 0; t < spec.Tracks; t++ {
		video := t%2 == 0
		sizes[t] = make([]int, spec.Samples)
		offsets[t] = make([]int64, spec.Samples)
		for i := 0; i < spec.Samples; i++ {
			sizes[t][i] = sampleSize(video, i+t)
			offsets[t][i] = payload
			payload += int64(sizes[t][i])
		}
	}

	b := &byteBuf{}
	b.box("ftyp", func() {
		b.fourcc("isom")
		b.u32(0x200)
		b.fourcc("isom")
		b.fourcc("iso2")
		b.fourcc("avc1")
		b.fourcc("mp41")
	})

	// Sample description tables are written with offsets relative to the
	// moov start; they are patched once the moov size is known.
	var stcoPatches []stcoPatch
	moovStart := len(b.b)

	b.box("moov", func() {
		b.box("mvhd", func() {
			b.fullbox(0, 0)
			b.u32(0)
			b.u32(0)
			b.u32(1000)
			b.u32(uint32(spec.Samples * 100))
			b.u32(0x00010000)
			b.u16(0x0100)
			b.u16(0)
			b.zeros(8)
			b.matrix(spec.Rotation)
			b.zeros(24)
			b.u32(uint32(spec.Tracks + 1))
		})
		for t := 0; t < spec.Tracks; t++ {
			writeTrak(b, spec, t, sizes[t], &stcoPatches)
		}
		for i := 0; i < spec.UnknownBoxes; i++ {
			b.box("skip", func() {
				b.u32(uint32(i))
				b.zeros(12)
			})
		}
	})
	moovEnd := len(b.b)

	// mdat header immediately follows moov; its payload starts after it.
	mdatHeader := moovEnd
	payloadBase := int64(mdatHeader + 8)
	b.u32(uint32(8 + payload))
	b.fourcc("mdat")

	for _, p := range stcoPatches {
		abs := payloadBase + offsets[p.track][p.sample]
		if abs > int64(^uint32(0)) {
			return nil, fmt.Errorf("fixture exceeds 32-bit chunk offsets")
		}
		binary.BigEndian.PutUint32(b.b[p.at:], uint32(abs))
	}
	for t := range offsets {
		for i := range offsets[t] {
			offsets[t][i] += payloadBase
		}
	}

	if spec.Corrupt == "sample_length" {
		if err := patchSampleLength(b.b, moovStart, spec); err != nil {
			return nil, err
		}
	}

	layout := &MP4Layout{
		Prefix:        b.b,
		PayloadSize:   payload,
		SampleOffsets: offsets,
		SampleSizes:   sizes,
	}
	if spec.Corrupt == "truncate" {
		if spec.Offset > layout.TotalSize() {
			return nil, fmt.Errorf("truncate offset %d is beyond the %d-byte fixture",
				spec.Offset, layout.TotalSize())
		}
		// The file is cut after it is written; the surviving boxes keep their
		// declared sizes so the parser must compare them with the real EOF.
		layout.TruncateAt = spec.Offset
	}
	return layout, nil
}

type stcoPatch struct {
	track  int
	sample int // -1 marks a placeholder for the chunk offset of sample 0
	at     int // absolute index into the prefix buffer
}

func writeTrak(b *byteBuf, spec MP4Spec, t int, sizes []int, patches *[]stcoPatch) {
	video := t%2 == 0
	timescale := uint32(30000)
	delta := uint32(1001)
	if !video {
		timescale = 48000
		delta = 1024
	}
	duration := uint32(len(sizes)) * delta

	b.box("trak", func() {
		b.box("tkhd", func() {
			b.fullbox(0, 3)
			b.u32(0)
			b.u32(0)
			b.u32(uint32(t + 1))
			b.u32(0)
			b.u32(duration * 1000 / timescale)
			b.zeros(8)
			b.u16(0)
			b.u16(0)
			if video {
				b.u16(0)
			} else {
				b.u16(0x0100)
			}
			b.u16(0)
			b.matrix(0)
			if video {
				b.u32(1920 << 16)
				b.u32(1080 << 16)
			} else {
				b.u32(0)
				b.u32(0)
			}
		})
		b.box("mdia", func() {
			b.box("mdhd", func() {
				b.fullbox(0, 0)
				b.u32(0)
				b.u32(0)
				b.u32(timescale)
				b.u32(duration)
				b.u16(0x55c4)
				b.u16(0)
			})
			b.box("hdlr", func() {
				b.fullbox(0, 0)
				b.u32(0)
				if video {
					b.fourcc("vide")
				} else {
					b.fourcc("soun")
				}
				b.zeros(12)
				b.u8(0)
			})
			b.box("minf", func() {
				b.box("stbl", func() {
					writeStsd(b, spec, video)
					b.box("stts", func() {
						b.fullbox(0, 0)
						b.u32(1)
						b.u32(uint32(len(sizes)))
						b.u32(delta)
					})
					if spec.CTTS {
						b.box("ctts", func() {
							b.fullbox(0, 0)
							b.u32(2)
							b.u32(1)
							b.u32(2 * delta)
							b.u32(uint32(len(sizes) - 1))
							b.u32(0)
						})
					}
					// One chunk per sample keeps offsets trivially
					// verifiable while still exercising stsc.
					b.box("stsc", func() {
						b.fullbox(0, 0)
						b.u32(1)
						b.u32(1)
						b.u32(1)
						b.u32(1)
					})
					b.box("stsz", func() {
						b.fullbox(0, 0)
						b.u32(0)
						b.u32(uint32(len(sizes)))
						for _, s := range sizes {
							b.u32(uint32(s))
						}
					})
					b.box("stco", func() {
						b.fullbox(0, 0)
						b.u32(uint32(len(sizes)))
						for i := range sizes {
							*patches = append(*patches, stcoPatch{
								track: t, sample: i, at: len(b.b),
							})
							b.u32(0)
						}
					})
				})
			})
		})
	})
}

func writeStsd(b *byteBuf, spec MP4Spec, video bool) {
	b.box("stsd", func() {
		b.fullbox(0, 0)
		b.u32(1)
		if video {
			b.box("avc1", func() {
				b.zeros(6)
				b.u16(1)
				b.u16(0)
				b.u16(0)
				b.zeros(12)
				b.u16(1920)
				b.u16(1080)
				b.u32(0x00480000)
				b.u32(0x00480000)
				b.u32(0)
				b.u16(1)
				b.zeros(32)
				b.u16(0x0018)
				b.u16(0xffff)
				b.box("avcC", func() {
					b.u8(1)
					b.u8(0x64)
					b.u8(0)
					b.u8(0x1f)
					b.u8(0xff)
					b.u8(0xe1)
					b.u16(4)
					b.fourcc("SPS1")
					b.u8(1)
					b.u16(2)
					b.fourcc("PP")
				})
				if c := spec.Color; c != nil {
					b.box("colr", func() {
						b.fourcc("nclx")
						b.u16(c.Primaries)
						b.u16(c.Transfer)
						b.u16(c.Matrix)
						if c.FullRange {
							b.u8(0x80)
						} else {
							b.u8(0)
						}
					})
				}
				if c := spec.Crop; c != nil {
					b.box("clap", func() {
						for _, v := range []int{c.Width, c.Height, c.X, c.Y} {
							b.u32(uint32(v << 16))
						}
					})
				}
			})
		} else {
			b.box("mp4a", func() {
				b.zeros(6)
				b.u16(1)
				b.u16(0)
				b.u16(0)
				b.u32(0)
				b.u16(2)
				b.u16(16)
				b.u16(0)
				b.u16(0)
				b.u32(48000 << 16)
			})
		}
	})
}

func patchSampleLength(prefix []byte, moovStart int, spec MP4Spec) error {
	// Locate the stsz entry for the requested sample by scanning the prefix
	// for the stsz box and computing its absolute entry offset.
	want := int64(spec.Offset)
	var found int
	idx := moovStart
	for {
		i := indexBox(prefix, idx, "stsz")
		if i < 0 {
			break
		}
		payload := i + 8
		if payload+12 > len(prefix) {
			break
		}
		count := int(binary.BigEndian.Uint32(prefix[payload+8:]))
		entryAt := payload + 12 + int(want)*4
		if want < int64(count) && entryAt+4 <= len(prefix) {
			binary.BigEndian.PutUint32(prefix[entryAt:], uint32(96<<20))
			found++
			return nil
		}
		idx = i + 8
	}
	if found == 0 {
		return fmt.Errorf("could not locate stsz entry to corrupt")
	}
	return nil
}

func indexBox(prefix []byte, from int, typ string) int {
	for i := from; i+8 <= len(prefix); {
		size := int(binary.BigEndian.Uint32(prefix[i:]))
		if size < 8 || i+size > len(prefix) {
			if string(prefix[i+4:i+8]) == typ {
				return i
			}
			i++
			continue
		}
		if string(prefix[i+4:i+8]) == typ {
			return i
		}
		i += size
	}
	return -1
}

// WriteMP4File materializes a fixture on disk. The payload is a sparse hole,
// so a 1 GiB fixture consumes almost no physical space.
func WriteMP4File(path string, layout *MP4Layout) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(layout.Prefix); err != nil {
		return err
	}
	if layout.TruncateAt > 0 {
		return f.Truncate(layout.TruncateAt)
	}
	return f.Truncate(layout.TotalSize())
}
