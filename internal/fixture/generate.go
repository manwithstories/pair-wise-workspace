// Package fixture generates the self-contained sample media used by the
// demo and the tests: a simplified MXF (CDCI + JPEG frames) and a
// simplified MP4 (tkhd/clap/colr + PCM packets + XOR parity).
package fixture

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"os"

	"archive/internal/ingest"
	"archive/internal/media"
	"archive/internal/recovery"
)

// Track numbers used across all generated fixtures.
const (
	VideoTrack = 1
	AudioTrack = 2
)

// Fixture geometry: 90 video frames over ~3 s, 141 PCM packets of
// 1024 samples at 48 kHz.
const (
	Frames       = 90
	AudioPackets = 141
	SamplesPer   = 1024
	imgW, imgH   = 64, 48
)

// Options selects the generated scenario.
type Options struct {
	Case string // "clean", "damaged" or "invalid"
	Seed int64  // perturbs frame pixels and PCM content deterministically
}

// Plan describes a generated fixture pair.
type Plan struct {
	MXFPath  string
	MP4Path  string
	LostSeqs []int64 // audio sequence numbers omitted from the MP4
}

// driftNum/driftDen is the damaged-case clock drift: +300 ppm (the local
// clock runs fast), inside the ±500 ppm correction limit.
const (
	driftNum = int64(1_000_300)
	driftDen = int64(1_000_000)
)

// damagedLosses: 18 scattered single losses (each in its own parity
// group) plus a 2-packet burst in one group → 18/20 recoverable.
func damagedLosses() []int64 {
	var out []int64
	for k := int64(0); k < 18; k++ {
		out = append(out, 8+6*k) // groups 2,3,5,…,27 — all distinct
	}
	return append(out, 121, 122) // burst inside group 30
}

// Generate writes the fixture pair into dir and returns its plan.
func Generate(dir string, opts Options) (*Plan, error) {
	if opts.Case == "" {
		opts.Case = "clean"
	}
	switch opts.Case {
	case "clean", "damaged", "invalid":
	default:
		return nil, fmt.Errorf("unknown case %q", opts.Case)
	}
	mxfPath, err := ingest.SafeJoin(dir, "video.mxf")
	if err != nil {
		return nil, err
	}
	mp4Path, err := ingest.SafeJoin(dir, "audio.mp4")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(mxfPath, buildMXF(opts), 0o600); err != nil {
		return nil, err
	}
	mp4, lost := buildMP4(opts)
	if err := os.WriteFile(mp4Path, mp4, 0o600); err != nil {
		return nil, err
	}
	return &Plan{MXFPath: mxfPath, MP4Path: mp4Path, LostSeqs: lost}, nil
}

// frameJPEG renders a deterministic 64x48 JPEG for frame f.
func frameJPEG(f int, seed int64) []byte {
	img := image.NewNRGBA(image.Rect(0, 0, imgW, imgH))
	for y := 0; y < imgH; y++ {
		for x := 0; x < imgW; x++ {
			img.SetNRGBA(x, y, color.NRGBA{
				R: uint8(x*4 + f*3 + int(seed)),
				G: uint8(y*5 + f*2),
				B: uint8(x + y + f),
				A: 0xFF,
			})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 85}); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

func buildMXF(opts Options) []byte {
	var out []byte
	// CDCI descriptor: BT.709, rotation flag 0 (the damaged MP4 carries a
	// conflicting tkhd rotation to exercise matrix-wins resolution).
	out = klvAppend(out, keyCDCI, cdciValue(imgW, imgH, 1, 1, 1, 0))
	const modulus = int64(1) << 33
	var pts int64
	if opts.Case != "clean" {
		pts = modulus - 150_000 // force a 33-bit wrap mid-stream
	}
	vfr := []int64{3000, 4500, 1500} // damaged-case VFR durations (90 kHz)
	for f := 0; f < Frames; f++ {
		dur := int64(3000)
		if opts.Case != "clean" {
			dur = vfr[f%len(vfr)]
		}
		out = klvAppend(out, keyFrame, frameValue(pts%modulus, uint32(dur), frameJPEG(f, opts.Seed)))
		pts += dur
	}
	return out
}

func buildMP4(opts Options) ([]byte, []int64) {
	damaged := opts.Case == "damaged"
	drift := func(ref int64) int64 {
		if damaged {
			return ref * driftNum / driftDen
		}
		return ref
	}
	var out []byte
	out = boxAppend(out, "ftyp", []byte("isom"))
	const one = 0x00010000
	matrix := [9]int32{one, 0, 0, 0, one, 0, 0, 0, 0x40000000}
	if damaged {
		matrix = [9]int32{0, one, 0, -one, 0, 0, 0, 0, 0x40000000} // 90°
	}
	out = boxAppend(out, "tkhd", tkhdMatrix(matrix))
	switch opts.Case {
	case "clean":
		out = boxAppend(out, "clap", clapValue(4, 4, 56, 40))
	case "invalid":
		out = boxAppend(out, "clap", clapValue(0, 0, 999, 999)) // out of bounds
	}
	prim := byte(1)
	if damaged {
		prim = 5 // conflicts with CDCI BT.709; descriptor priority decides
	}
	out = boxAppend(out, "colr", []byte{prim, prim, prim})
	var anch [][2]int64
	for i := int64(0); i <= 5; i++ {
		ref := i * 600_000_000
		anch = append(anch, [2]int64{drift(ref), ref})
	}
	out = boxAppend(out, "anch", anchorValue(anch))

	var lost []int64
	if damaged {
		lost = damagedLosses()
	}
	isLost := func(seq int64) bool {
		for _, s := range lost {
			if s == seq {
				return true
			}
		}
		return false
	}
	// PCM payloads and parity are computed over the original (lossless)
	// packet set; lost apkt boxes are then simply omitted.
	pcm := make([][]byte, AudioPackets)
	pts := make([]int64, AudioPackets)
	for i := 0; i < AudioPackets; i++ {
		ref := int64(i) * SamplesPer * media.NanosPerSecond / media.AudioRate
		pts[i] = drift(ref)
		p := make([]byte, SamplesPer)
		for j := range p {
			p[j] = byte(i*31 + j*7 + int(opts.Seed))
		}
		pcm[i] = p
	}
	for g := 0; g < AudioPackets; g += recovery.GroupSize {
		end := g + recovery.GroupSize
		if end > AudioPackets {
			end = AudioPackets
		}
		var payloads [][]byte
		var gpts []int64
		var samples, lengths []int
		for i := g; i < end; i++ {
			payloads = append(payloads, pcm[i])
			gpts = append(gpts, pts[i])
			samples = append(samples, SamplesPer)
			lengths = append(lengths, len(pcm[i]))
		}
		for i := g; i < end; i++ {
			if !isLost(int64(i)) {
				out = boxAppend(out, "apkt", apktValue(int64(i), pts[i], SamplesPer, pcm[i]))
			}
		}
		out = boxAppend(out, "par1", par1Value(int64(g), gpts, samples, lengths,
			recovery.XorPayloads(payloads)))
	}
	if damaged {
		// Truncated trailing clap: declared 24 bytes, only 10 present.
		out = append(out, boxAppend(nil, "clap", clapValue(0, 0, 10, 10))[:10]...)
	}
	return out, lost
}
