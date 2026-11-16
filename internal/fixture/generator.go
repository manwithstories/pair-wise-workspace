package fixture

import (
	"fmt"
	"os"
	"path/filepath"
)

// Timing constants for generated fixtures.
const (
	VideoTimescale = 18000 // 18 fps at delta 1000
	VideoDelta     = 1000
	AudioTimescale = 48000
	AudioDelta     = 1024
	MovieTimescale = 1000
	KeyInterval    = 10
	VideoPerChunk  = 10
	AudioPerChunk  = 20
)

// Params controls fixture generation.
type Params struct {
	Out           string
	VideoFrames   int
	AudioFrames   int
	EditList      bool
	TimestampWrap bool
	Seed          int64
	Inject        string // "", "truncate", "bad-offset", "no-stss", "bad-elst"
}

// Generate writes a deterministic MP4 fixture per p.
func Generate(p Params) error {
	if p.Out == "" {
		return fmt.Errorf("fixture: --out is required")
	}
	if p.VideoFrames <= 0 || p.AudioFrames <= 0 {
		return fmt.Errorf("fixture: frame counts must be positive")
	}
	switch p.Inject {
	case "", "truncate", "bad-offset", "no-stss", "bad-elst":
	default:
		return fmt.Errorf("fixture: unknown --inject %q", p.Inject)
	}

	rnd := newPRNG(p.Seed)
	vSizes := make([]int, p.VideoFrames)
	for i := range vSizes {
		vSizes[i] = rnd.rangeInt(2000, 9000)
	}
	aSizes := make([]int, p.AudioFrames)
	for i := range aSizes {
		aSizes[i] = rnd.rangeInt(200, 600)
	}

	// Assemble mdat with interleaved chunks; record chunk offsets
	// relative to the mdat payload start.
	var mdat []byte
	var vChunks, aChunks []int64
	vi, ai := 0, 0
	for vi < p.VideoFrames || ai < p.AudioFrames {
		if vi < p.VideoFrames {
			vChunks = append(vChunks, int64(len(mdat)))
			for k := 0; k < VideoPerChunk && vi < p.VideoFrames; k++ {
				buf := make([]byte, vSizes[vi])
				rnd.bytes(buf)
				mdat = append(mdat, buf...)
				vi++
			}
		}
		if ai < p.AudioFrames {
			aChunks = append(aChunks, int64(len(mdat)))
			for k := 0; k < AudioPerChunk && ai < p.AudioFrames; k++ {
				buf := make([]byte, aSizes[ai])
				rnd.bytes(buf)
				mdat = append(mdat, buf...)
				ai++
			}
		}
	}

	// ftyp is 24 bytes; mdat header is 8. Chunk absolute offsets:
	const ftypSize = 24
	const mdatHeader = 8
	abs := func(rel []int64) []int64 {
		out := make([]int64, len(rel))
		for i, r := range rel {
			out[i] = ftypSize + mdatHeader + r
		}
		return out
	}
	vOff, aOff := abs(vChunks), abs(aChunks)
	if p.Inject == "bad-offset" && len(vOff) > 0 {
		vOff[0] = 0xFFFFF000 // points far past EOF
	}

	g := &gen{p: p, vSizes: vSizes, aSizes: aSizes, vOff: vOff, aOff: aOff}
	moov := &boxWriter{}
	moov.container("moov", func(w *boxWriter) {
		g.mvhd(w)
		g.videoTrak(w)
		g.audioTrak(w)
	})

	ftyp := &boxWriter{}
	ftyp.box("ftyp", concat([]byte("isom"), u32(0x200), []byte("isom"), []byte("iso2")))
	mdatW := &boxWriter{}
	mdatW.box("mdat", mdat)

	if err := os.MkdirAll(filepath.Dir(p.Out), 0o755); err != nil {
		return err
	}
	f, err := os.Create(p.Out)
	if err != nil {
		return err
	}
	if _, err := f.Write(ftyp.buf.Bytes()); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(mdatW.buf.Bytes()); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(moov.buf.Bytes()); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if p.Inject == "truncate" {
		fi, err := os.Stat(p.Out)
		if err != nil {
			return err
		}
		if err := os.Truncate(p.Out, fi.Size()*3/5); err != nil {
			return err
		}
	}
	return nil
}

type gen struct {
	p              Params
	vSizes, aSizes []int
	vOff, aOff     []int64
}

// videoMediaTime returns the elst media_time origin for the video track.
func (g *gen) videoMediaTime() int64 {
	if g.p.TimestampWrap {
		return (int64(1) << 32) - 3*VideoDelta // crosses the 32-bit boundary
	}
	return VideoDelta // non-zero start: one frame in
}

// videoPresentation returns (emptyGapMs, mediaDurMs) for the edit list.
func (g *gen) videoPresentation() (gap, mediaDur int64) {
	mediaTotal := int64(g.p.VideoFrames) * VideoDelta
	origin := g.videoMediaTime()
	if g.p.TimestampWrap {
		origin = 0 // duration is measured against the unwrapped media extent
	}
	mediaDur = (mediaTotal - origin) * MovieTimescale / VideoTimescale
	if g.p.EditList {
		gap = 500 // 0.5s empty leading segment
	}
	return gap, mediaDur
}

func (g *gen) mvhd(w *boxWriter) {
	gap, mediaDur := g.videoPresentation()
	dur := gap + mediaDur
	payload := concat(
		u32(0), u32(0), u32(MovieTimescale), u32(uint32(dur)),
		u32(0x00010000), u16(0x0100), u16(0), make([]byte, 8),
		u32(0x10000), u32(0), u32(0), u32(0), u32(0x10000), u32(0),
		u32(0), u32(0), u32(0x40000000),
		make([]byte, 24), u32(3),
	)
	w.fullBox("mvhd", 0, 0, payload)
}

func (g *gen) tkhd(w *boxWriter, id uint32, durMs int64, width, height uint32) {
	payload := concat(
		u32(0), u32(0), u32(id), u32(0), u32(uint32(durMs)),
		make([]byte, 8), u16(0), u16(0), u16(0), u16(0),
		u32(0x10000), u32(0), u32(0), u32(0), u32(0x10000), u32(0),
		u32(0), u32(0), u32(0x40000000),
		u32(width<<16), u32(height<<16),
	)
	w.fullBox("tkhd", 0, 0x3, payload)
}

func mdhdBox(w *boxWriter, timescale, duration uint32) {
	payload := concat(u32(0), u32(0), u32(timescale), u32(duration), u16(0x55c4), u16(0))
	w.fullBox("mdhd", 0, 0, payload)
}

func hdlrBox(w *boxWriter, handler, name string) {
	payload := concat(u32(0), []byte(handler), make([]byte, 12), append([]byte(name), 0))
	w.fullBox("hdlr", 0, 0, payload)
}

func dinfBox(w *boxWriter) {
	w.container("dinf", func(w *boxWriter) {
		sub := &boxWriter{}
		sub.fullBox("url ", 0, 1, nil)
		w.fullBox("dref", 0, 0, concat(u32(1), sub.buf.Bytes()))
	})
}

func stscBox(w *boxWriter, perChunk uint32) {
	w.fullBox("stsc", 0, 0, concat(u32(1), u32(1), u32(perChunk), u32(1)))
}

func stszBox(w *boxWriter, sizes []int) {
	payload := concat(u32(0), u32(uint32(len(sizes))))
	for _, s := range sizes {
		payload = append(payload, u32(uint32(s))...)
	}
	w.fullBox("stsz", 0, 0, payload)
}

func stcoBox(w *boxWriter, offsets []int64) {
	payload := u32(uint32(len(offsets)))
	for _, o := range offsets {
		payload = append(payload, u32(uint32(o))...)
	}
	w.fullBox("stco", 0, 0, payload)
}
