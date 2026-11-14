// Package fixture synthesizes YUV420p master segments and a manifest.
package fixture

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"os"
	"path/filepath"

	"tcr/internal/yuv"
)

// Segment describes one generated master segment.
type Segment struct {
	Index      int    `json:"index"`
	Name       string `json:"name"`
	File       string `json:"file"`
	StartFrame int    `json:"start_frame"`
	Frames     int    `json:"frames"`
}

// Manifest describes a generated master set.
type Manifest struct {
	Width    int       `json:"width"`
	Height   int       `json:"height"`
	FPS      int       `json:"fps"`
	Frames   int       `json:"frames"`
	Segments []Segment `json:"segments"`
}

var segNames = []string{"gradient", "title", "noise", "pan", "cuts", "mixed"}

// Generate creates segs synthetic master segments under outDir plus manifest.json.
func Generate(outDir string, segs, w, h, frames, fps int) (*Manifest, error) {
	if segs < 1 {
		return nil, fmt.Errorf("segs must be >= 1, got %d", segs)
	}
	if frames < segs {
		return nil, fmt.Errorf("frames (%d) must be >= segs (%d): every segment needs at least one frame", frames, segs)
	}
	if fps <= 0 {
		return nil, fmt.Errorf("fps must be positive, got %d", fps)
	}
	if _, err := yuv.NewFrame(w, h); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, err
	}
	m := &Manifest{Width: w, Height: h, FPS: fps, Frames: frames}
	base, rem := frames/segs, frames%segs
	start := 0
	for i := 0; i < segs; i++ {
		n := base
		if i == segs-1 {
			n += rem
		}
		name := segNames[i%len(segNames)]
		file := fmt.Sprintf("seg_%02d_%s.yuv", i, name)
		seg := Segment{Index: i, Name: name, File: file, StartFrame: start, Frames: n}
		if err := writeSegment(filepath.Join(outDir, file), i, w, h, n); err != nil {
			return nil, err
		}
		m.Segments = append(m.Segments, seg)
		start += n
	}
	if err := m.Save(filepath.Join(outDir, "manifest.json")); err != nil {
		return nil, err
	}
	return m, nil
}

// Save writes the manifest as indented JSON.
func (m *Manifest) Save(path string) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// LoadManifest reads a manifest JSON file.
func LoadManifest(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("manifest not found: %s", path)
		}
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("manifest %q is not valid JSON: %w", path, err)
	}
	if _, err := yuv.NewFrame(m.Width, m.Height); err != nil {
		return nil, fmt.Errorf("manifest %q: %w", path, err)
	}
	if m.Frames <= 0 {
		return nil, fmt.Errorf("manifest %q: frames must be positive, got %d", path, m.Frames)
	}
	if m.FPS <= 0 {
		return nil, fmt.Errorf("manifest %q: fps must be positive, got %d", path, m.FPS)
	}
	if len(m.Segments) == 0 {
		return nil, fmt.Errorf("manifest %q: no segments listed", path)
	}
	for _, s := range m.Segments {
		if s.Frames <= 0 {
			return nil, fmt.Errorf("manifest %q: segment %d (%s) has zero frames", path, s.Index, s.Name)
		}
		if s.StartFrame < 0 || s.StartFrame+s.Frames > m.Frames {
			return nil, fmt.Errorf("manifest %q: segment %d (%s) frames [%d,%d) exceed total %d",
				path, s.Index, s.Name, s.StartFrame, s.StartFrame+s.Frames, m.Frames)
		}
	}
	return &m, nil
}

func writeSegment(path string, kind, w, h, frames int) error {
	fh, err := os.Create(path)
	if err != nil {
		return err
	}
	defer fh.Close()
	rnd := rand.New(rand.NewSource(int64(kind)*7919 + 42))
	gen := generators[kind%len(generators)]
	for t := 0; t < frames; t++ {
		f, err := yuv.NewFrame(w, h)
		if err != nil {
			return err
		}
		gen(f, t, rnd)
		if _, err := f.WriteTo(fh); err != nil {
			return err
		}
	}
	return nil
}

func clampByte(v float64) byte {
	if v < 16 {
		return 16
	}
	if v > 235 {
		return 235
	}
	return byte(v + 0.5)
}

func fillChroma(f *yuv.Frame) {
	for i := range f.Cb {
		f.Cb[i] = 128
		f.Cr[i] = 128
	}
}

// addGrain overlays per-frame gaussian grain so every segment carries
// a comparable, non-trivial entropy floor.
func addGrain(f *yuv.Frame, rnd *rand.Rand, sigma float64) {
	for i := range f.Y {
		f.Y[i] = clampByte(float64(f.Y[i]) + rnd.NormFloat64()*sigma)
	}
}

type genFunc func(f *yuv.Frame, t int, rnd *rand.Rand)

var generators = []genFunc{genGradient, genTitle, genNoise, genPan, genCuts, genMixed}

// genGradient: smooth horizontal ramp drifting slowly over time.
func genGradient(f *yuv.Frame, t int, rnd *rand.Rand) {
	for y := 0; y < f.H; y++ {
		for x := 0; x < f.W; x++ {
			v := 40 + 150*float64(x)/float64(f.W-1) + 6*math.Sin(float64(t)*0.35+float64(y)*0.02)
			f.Y[y*f.W+x] = clampByte(v)
		}
	}
	addGrain(f, rnd, 5)
	fillChroma(f)
}

// genTitle: near-static dark card with bright subtitle-like bars.
func genTitle(f *yuv.Frame, t int, rnd *rand.Rand) {
	for i := range f.Y {
		f.Y[i] = 24
	}
	flicker := float64(t % 2)
	for row := 0; row < 3; row++ {
		y0 := f.H*7/10 + row*(f.H/12)
		x0, x1 := f.W/5, f.W*4/5-row*(f.W/10)
		for y := y0; y < y0+f.H/24 && y < f.H; y++ {
			for x := x0; x < x1 && x < f.W; x++ {
				f.Y[y*f.W+x] = clampByte(215 + flicker)
			}
		}
	}
	for y := f.H / 12; y < f.H/12+f.H/10; y++ {
		for x := f.W / 16; x < f.W/16+f.W/8; x++ {
			f.Y[y*f.W+x] = 180
		}
	}
	addGrain(f, rnd, 5)
	fillChroma(f)
}

// genNoise: mid-gray with per-frame gaussian grain.
func genNoise(f *yuv.Frame, t int, rnd *rand.Rand) {
	for i := range f.Y {
		f.Y[i] = clampByte(120 + rnd.NormFloat64()*8)
	}
	fillChroma(f)
}

// genPan: vertical stripes shifting fast to the left (camera pan).
func genPan(f *yuv.Frame, t int, rnd *rand.Rand) {
	shift := t * 5
	for y := 0; y < f.H; y++ {
		for x := 0; x < f.W; x++ {
			stripe := ((x + shift) / 16) % 2
			v := 60 + 130*float64(stripe) + 20*float64(y)/float64(f.H-1)
			f.Y[y*f.W+x] = clampByte(v)
		}
	}
	addGrain(f, rnd, 5)
	fillChroma(f)
}

// genCuts: abrupt scene switches every two frames.
func genCuts(f *yuv.Frame, t int, rnd *rand.Rand) {
	scene := (t / 2) % 3
	for y := 0; y < f.H; y++ {
		for x := 0; x < f.W; x++ {
			var v float64
			switch scene {
			case 0:
				v = 50 + 150*float64((x/20)%2)
			case 1:
				v = 40 + 160*float64(((x/16)+(y/16))%2)
			default:
				v = 30 + 190*float64(y)/float64(f.H-1)
			}
			f.Y[y*f.W+x] = clampByte(v)
		}
	}
	addGrain(f, rnd, 5)
	fillChroma(f)
}

// genMixed: bouncing bright square over a slowly fading background.
func genMixed(f *yuv.Frame, t int, rnd *rand.Rand) {
	bg := 90 + 20*math.Sin(float64(t)*0.2)
	for i := range f.Y {
		f.Y[i] = clampByte(bg)
	}
	sz := f.W / 8
	sx := (t * 7) % (f.W - sz)
	sy := int(math.Abs(math.Sin(float64(t)*0.5)) * float64(f.H-sz))
	for y := sy; y < sy+sz; y++ {
		for x := sx; x < sx+sz; x++ {
			f.Y[y*f.W+x] = 210
		}
	}
	addGrain(f, rnd, 5)
	fillChroma(f)
}
