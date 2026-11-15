// Package source creates deterministic YUV420 test clips without external assets.
package source

import (
	"fmt"
	"hash/fnv"
	"math"
)

// Frame is a planar YUV420 frame. U and V are half resolution in both axes.
type Frame struct {
	Index  int
	Width  int
	Height int
	Y      []byte
	U      []byte
	V      []byte
}

// Clone returns a deep copy suitable for storing a reconstructed reference.
func (f Frame) Clone() Frame {
	return Frame{
		Index: f.Index, Width: f.Width, Height: f.Height,
		Y: append([]byte(nil), f.Y...),
		U: append([]byte(nil), f.U...),
		V: append([]byte(nil), f.V...),
	}
}

// Synth generates a fixed, seed-driven clip. Access is safe for concurrent use.
type Synth struct {
	Clip   string
	Frames int
	Width  int
	Height int
	Seed   int64
}

// New validates dimensions and returns a deterministic source.
func New(clip string, frames, width, height int, seed int64) (*Synth, error) {
	if clip == "" {
		clip = "news"
	}
	if frames < 0 {
		return nil, fmt.Errorf("frames must be non-negative")
	}
	if width < 16 || height < 16 || width%8 != 0 || height%8 != 0 {
		return nil, fmt.Errorf("width and height must be multiples of 8 and at least 16")
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(clip))
	return &Synth{Clip: clip, Frames: frames, Width: width, Height: height, Seed: seed ^ int64(h.Sum64())}, nil
}

// Scene returns the synthetic scene index. A new scene starts every 60 frames.
func Scene(frame int) int { return frame / 60 }

// HardCut reports whether frame starts a new scene.
func HardCut(frame int) bool { return frame > 0 && frame%60 == 0 }

// At returns frame index. It never mutates the generator.
func (s *Synth) At(index int) (Frame, error) {
	if index < 0 || index >= s.Frames {
		return Frame{}, fmt.Errorf("frame %d outside [0,%d)", index, s.Frames)
	}
	f := Frame{Index: index, Width: s.Width, Height: s.Height,
		Y: make([]byte, s.Width*s.Height),
		U: make([]byte, (s.Width/2)*(s.Height/2)),
		V: make([]byte, (s.Width/2)*(s.Height/2))}
	scene := Scene(index)
	sx := int(hash32(uint64(s.Seed), uint64(scene), 11) % 97)
	sy := int(hash32(uint64(s.Seed), uint64(scene), 29) % 61)
	dark := index >= 90 && index < 110
	boxW, boxH := s.Width/4, s.Height/3
	boxX := (12 + index*2 + sx) % max(1, s.Width-boxW-12)
	boxY := s.Height/2 - boxH/2 + int(10*math.Sin(float64(index)/9+float64(sy)))
	for y := 0; y < s.Height; y++ {
		for x := 0; x < s.Width; x++ {
			base := 48.0 + float64(scene%4)*18 + float64(x*54/s.Width) + float64(y*28/s.Height)
			wave := 16 * math.Sin(float64(x+index*2)/13+float64(sy)) * math.Cos(float64(y-index)/17)
			v := base + wave + float64(int(hash32(uint64(x), uint64(y+index*7), uint64(scene)+uint64(s.Seed))%17)-8)
			if x >= boxX && x < boxX+boxW && y >= boxY && y < boxY+boxH {
				v = 118 + float64((x+y+index*3)%95)
				if ((x-boxX)/4+(y-boxY)/4)%2 == 0 {
					v += 42
				}
			}
			if dark {
				grain := float64(int(hash32(uint64(x*3), uint64(y*5+index), uint64(scene))%15) - 7)
				v = 5 + v*0.11 + grain
			}
			f.Y[y*s.Width+x] = clamp8(v)
		}
	}
	for y := 0; y < s.Height/2; y++ {
		for x := 0; x < s.Width/2; x++ {
			cx, cy := x*2, y*2
			inside := cx >= boxX && cx < boxX+boxW && cy >= boxY && cy < boxY+boxH
			u := 112 + 26*math.Sin(float64(cx+scene*9)/19)
			v := 126 + 22*math.Cos(float64(cy-scene*7)/23)
			if inside {
				u, v = 78+float64((x+scene)%31), 166-float64((y+scene)%27)
			}
			if dark {
				u = 128 + (u-128)*0.2
				v = 128 + (v-128)*0.2
			}
			i := y*(s.Width/2) + x
			f.U[i], f.V[i] = clamp8(u), clamp8(v)
		}
	}
	return f, nil
}

func hash32(a, b, c uint64) uint32 {
	x := a*0x9e3779b185ebca87 ^ b*0xc2b2ae3d27d4eb4f ^ c*0x165667b19e3779f9
	x ^= x >> 33
	x *= 0xff51afd7ed558ccd
	x ^= x >> 33
	return uint32(x)
}

func clamp8(v float64) byte {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return byte(v + 0.5)
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
