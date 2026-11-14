package reel

import (
	"fmt"
	"math"
)

const (
	DefaultWidth        = 320
	DefaultHeight       = 180
	DefaultFPS          = 24
	DefaultFramesPerSeg = 24
)

// Generate builds the built-in scene script. Every byte is derived from the
// seed and segment/frame coordinates; no global math/rand state is involved.
func Generate(seed int64, segmentCount int) (*Bundle, error) {
	if segmentCount < 1 || segmentCount > 64 {
		return nil, fmt.Errorf("segment count must be in [1,64]")
	}
	b := &Bundle{
		Seed:         uint64(seed),
		Width:        DefaultWidth,
		Height:       DefaultHeight,
		FPS:          DefaultFPS,
		FramesPerSeg: DefaultFramesPerSeg,
		Segments:     make([][][]byte, segmentCount),
	}
	for si := 0; si < segmentCount; si++ {
		seg := make([][]byte, DefaultFramesPerSeg)
		scene := si % 12
		for fi := 0; fi < DefaultFramesPerSeg; fi++ {
			r := newRNG(mix64(uint64(seed), uint64(si+1), uint64(fi+1)))
			seg[fi] = generateFrame(scene, b.Width, b.Height, fi, r)
		}
		b.Segments[si] = seg
	}
	if err := b.Validate(); err != nil {
		return nil, err
	}
	return b, nil
}

func generateFrame(scene, w, h, fi int, r *rng) []byte {
	frame := make([]byte, w*h)
	pan := float64(fi)
	switch scene {
	case 0:
		generateStaticDialogue(frame, w, h, pan*0.05, r)
	case 1:
		generateGradientPan(frame, w, h, pan*1.6, 0.026)
	case 2:
		generateFastPan(frame, w, h, pan*5.0)
	case 3:
		generateParticles(frame, w, h, pan, r, 30, 1.0)
	case 4:
		generateTitle(frame, w, h, pan*0.03)
	case 5:
		generateStripes(frame, w, h, pan*2.2)
	case 6:
		generateDetailedPan(frame, w, h, pan*2.8)
	case 7:
		generateCloseup(frame, w, h, pan*0.9)
	case 8:
		generateAction(frame, w, h, pan*3.2, r)
	case 9:
		generateSoftGradient(frame, w, h, pan*0.04)
	case 10:
		generateComplexPan(frame, w, h, pan*5.5)
	default:
		generateNebula(frame, w, h, pan*1.2, r)
	}
	addFineTexture(frame, w, h)
	return frame
}

func addFineTexture(dst []byte, w, h int) {
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			texture := 12*math.Sin(float64(x)*0.23+float64(y)*0.11) +
				8*math.Cos(float64(x)*0.07-float64(y)*0.19)
			dst[y*w+x] = clampByte(float64(dst[y*w+x]) + texture)
		}
	}
}

func generateStaticDialogue(dst []byte, w, h int, drift float64, r *rng) {
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			v := 82 + 0.24*float64(x) + 0.10*float64(y)
			dst[y*w+x] = clampByte(v)
		}
	}
	for _, cx := range []int{w / 3, 2 * w / 3} {
		rect(dst, w, h, cx-25+int(drift), 76, 50, 66, 128)
		rect(dst, w, h, cx-18+int(drift), 55, 36, 30, 178)
	}
	rect(dst, w, h, 54, 145, w-108, 24, 58)
}

func generateGradientPan(dst []byte, w, h int, shift, freq float64) {
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			v := 118 + 30*math.Sin((float64(x)+shift)*freq) +
				22*math.Cos(float64(y)*0.045) + 12*math.Sin((float64(x+y)+shift)*freq*1.7)
			dst[y*w+x] = clampByte(v)
		}
	}
}

func generateFastPan(dst []byte, w, h int, shift float64) {
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			xx := x + int(shift)
			v := 106 + 52*math.Sin(float64(xx)*0.11) + 38*math.Sin(float64(y)*0.16) +
				24*math.Cos(float64(xx+y)*0.07)
			dst[y*w+x] = clampByte(v)
		}
	}
}

func generateParticles(dst []byte, w, h int, t float64, r *rng, count int, scale float64) {
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			dst[y*w+x] = clampByte(48 + 0.25*float64(x) + 12*math.Sin(float64(y)*0.03))
		}
	}
	for i := 0; i < count; i++ {
		x := int(float64(w)*r.float64()) + int(t*float64(i%5+1)*scale)
		y := int(float64(h)*r.float64()) + int(t*float64(i%3+1)*0.4*scale)
		rad := 1 + i%4
		disc(dst, w, h, x, y, rad, byte(190+i%40))
	}
}

func generateTitle(dst []byte, w, h int, flicker float64) {
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			v := 34 + 8*math.Sin(float64(x)*0.015) + 5*math.Cos(float64(y)*0.02) + flicker
			dst[y*w+x] = clampByte(v)
		}
	}
	softRect(dst, w, h, 62, 58, 196, 42, 5, 174)
	softRect(dst, w, h, 84, 66, 152, 8, 3, 58)
	softRect(dst, w, h, 84, 80, 124, 8, 3, 58)
	softRect(dst, w, h, 108, 116, 104, 6, 2, 144)
}

func generateStripes(dst []byte, w, h int, shift float64) {
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			phase := math.Mod(float64(x)+shift+float64(y)*0.6, 36)
			v := 90 + 54*math.Sin(phase*math.Pi/18)
			dst[y*w+x] = clampByte(v)
		}
	}
}

func generateDetailedPan(dst []byte, w, h int, shift float64) {
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			xx := x + int(shift)
			base := 112 + 23*math.Sin(float64(xx)*0.016) + 16*math.Cos(float64(y)*0.026)
			noise := float64(int(hash2D(uint32(xx), uint32(y), 0x51ed270b)&255)-128) * 0.13
			v := base + noise + 15*math.Sin(float64(xx+y)*0.060)
			dst[y*w+x] = clampByte(v)
		}
	}
}

func generateCloseup(dst []byte, w, h int, t float64) {
	cx := float64(w)/2 + 8*math.Sin(t*0.3)
	cy := float64(h)/2 + 3*math.Cos(t*0.4)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			dx, dy := float64(x)-cx, float64(y)-cy
			d := math.Sqrt(dx*dx + dy*dy)
			v := 62 + 22*math.Sin(d*0.055-t*0.35) + 0.20*float64(x)
			dst[y*w+x] = clampByte(v)
		}
	}
}

func generateAction(dst []byte, w, h int, shift float64, r *rng) {
	generateGradientPan(dst, w, h, shift, 0.038)
	for i := 0; i < 8; i++ {
		x := int(float64(w)*r.float64()) + int(shift*1.1)
		y := int(float64(h) * r.float64())
		disc(dst, w, h, x, y, 1+i%2, 195)
	}
}

func generateSoftGradient(dst []byte, w, h int, shift float64) {
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			v := 104 + 46*math.Sin(float64(x)*0.017+shift*0.02) +
				34*math.Cos(float64(y)*0.025) + 8*float64(x+y)/float64(w+h)
			dst[y*w+x] = clampByte(v)
		}
	}
}

func generateComplexPan(dst []byte, w, h int, shift float64) {
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			xx := x + int(shift)
			checker := float64(((xx/8)+(y/8))&1) * 16
			base := 88 + checker + 30*math.Sin(float64(xx)*0.046) + 18*math.Cos(float64(y)*0.067)
			noise := float64(int(hash2D(uint32(xx), uint32(y), 0x9e3779b9)&127)-64) * 0.14
			dst[y*w+x] = clampByte(base + noise)
		}
	}
}

func generateNebula(dst []byte, w, h int, t float64, r *rng) {
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			v := 32 + 18*math.Sin(float64(x)*0.012+t*0.1) + 15*math.Cos(float64(y)*0.02-t*0.2)
			dst[y*w+x] = clampByte(v)
		}
	}
	for i := 0; i < 16; i++ {
		x := int(float64(w)*r.float64()) + int(t*float64(i%4+1))
		y := int(float64(h)*r.float64()) + int(t*float64(i%2+1)*0.6)
		disc(dst, w, h, x, y, 1+i%2, byte(100+i*2))
	}
}

func rect(dst []byte, w, h, x, y, rw, rh int, value byte) {
	for yy := max(0, y); yy < min(h, y+rh); yy++ {
		for xx := max(0, x); xx < min(w, x+rw); xx++ {
			dst[yy*w+xx] = value
		}
	}
}

func softRect(dst []byte, w, h, x, y, rw, rh, feather int, value byte) {
	for yy := max(0, y-feather); yy < min(h, y+rh+feather); yy++ {
		for xx := max(0, x-feather); xx < min(w, x+rw+feather); xx++ {
			cx := edgeCoverage(xx, x, x+rw, feather)
			cy := edgeCoverage(yy, y, y+rh, feather)
			coverage := cx * cy
			if coverage <= 0 {
				continue
			}
			old := float64(dst[yy*w+xx])
			dst[yy*w+xx] = clampByte(old*(1-coverage) + float64(value)*coverage)
		}
	}
}

func edgeCoverage(v, start, end, feather int) float64 {
	if feather <= 0 {
		if v >= start && v < end {
			return 1
		}
		return 0
	}
	left := float64(v-(start-feather)) / float64(feather)
	right := float64((end+feather-1)-v) / float64(feather)
	c := math.Min(1, math.Min(left, right))
	if c < 0 {
		return 0
	}
	return c
}

func disc(dst []byte, w, h, cx, cy, radius int, value byte) {
	r2 := radius * radius
	for y := max(0, cy-radius); y < min(h, cy+radius+1); y++ {
		for x := max(0, cx-radius); x < min(w, cx+radius+1); x++ {
			dx, dy := x-cx, y-cy
			if dx*dx+dy*dy <= r2 {
				dst[y*w+x] = value
			}
		}
	}
}

func clampByte(v float64) byte {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return byte(v + 0.5)
}

type rng struct{ state uint64 }

func newRNG(seed uint64) *rng { return &rng{state: seed} }

func (r *rng) next() uint64 {
	r.state += 0x9e3779b97f4a7c15
	z := r.state
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

func (r *rng) float64() float64 { return float64(r.next()>>11) / float64(uint64(1)<<53) }

func mix64(a, b, c uint64) uint64 {
	x := a ^ (b * 0x9e3779b97f4a7c15) ^ (c * 0xbf58476d1ce4e5b9)
	x ^= x >> 30
	x *= 0xbf58476d1ce4e5b9
	x ^= x >> 27
	x *= 0x94d049bb133111eb
	return x ^ (x >> 31)
}

func hash2D(x, y, salt uint32) uint32 {
	h := x*0x8da6b343 + y*0xd8163841 + salt
	h ^= h >> 16
	h *= 0x7feb352d
	h ^= h >> 15
	return h
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
