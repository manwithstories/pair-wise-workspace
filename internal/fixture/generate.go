// Package fixture creates deterministic synthetic YUV420p turntable material.
// No external media or random source is used.
package fixture

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"

	"turncode/internal/config"
	"turncode/internal/yuv"
)

type Options struct {
	Width  int
	Height int
	FPS    int
	Frames int
	Scene  string
	Seed   int64
	OutDir string
}

var validScenes = map[string]bool{
	"mixed": true, "slow": true, "gradient": true, "grain": true, "cut": true,
}

// Generate writes a YUV stream and manifest, then returns the manifest spec.
func Generate(opt Options) (config.Spec, error) {
	if opt.Width < config.MinDimension || opt.Width > config.MaxWidth || opt.Width%2 != 0 {
		return config.Spec{}, fmt.Errorf("fixture: invalid width %d", opt.Width)
	}
	if opt.Height < config.MinDimension || opt.Height > config.MaxHeight || opt.Height%2 != 0 {
		return config.Spec{}, fmt.Errorf("fixture: invalid height %d", opt.Height)
	}
	if opt.FPS < config.MinFPS || opt.FPS > config.MaxFPS || opt.Frames < 1 || opt.Frames > config.MaxFrames {
		return config.Spec{}, fmt.Errorf("fixture: invalid fps/frames")
	}
	if !validScenes[opt.Scene] {
		return config.Spec{}, fmt.Errorf("fixture: unknown scene %q", opt.Scene)
	}
	if err := os.MkdirAll(opt.OutDir, 0o755); err != nil {
		return config.Spec{}, err
	}
	base := filepath.Base(filepath.Clean(opt.OutDir))
	if base == "." || base == string(filepath.Separator) || base == "" {
		return config.Spec{}, fmt.Errorf("fixture: invalid output directory")
	}
	yuvName := base + ".yuv"
	yuvPath := filepath.Join(opt.OutDir, yuvName)
	tmp, err := os.CreateTemp(opt.OutDir, ".fixture-*.yuv")
	if err != nil {
		return config.Spec{}, err
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		_ = tmp.Close()
		if !ok {
			_ = os.Remove(tmpName)
		}
	}()
	h := sha256.New()
	mw := io.MultiWriter(tmp, h)
	for i := 0; i < opt.Frames; i++ {
		f := renderFrame(opt, i)
		if err := yuv.WriteFrame(mw, f); err != nil {
			return config.Spec{}, err
		}
	}
	if err := tmp.Sync(); err != nil {
		return config.Spec{}, err
	}
	if err := tmp.Close(); err != nil {
		return config.Spec{}, err
	}
	if err := os.Rename(tmpName, yuvPath); err != nil {
		return config.Spec{}, err
	}
	ok = true
	frameBytes := config.ExpectedFrameBytes(opt.Width, opt.Height)
	spec := config.Spec{
		Version:       config.Version,
		Width:         opt.Width,
		Height:        opt.Height,
		FPS:           opt.FPS,
		Frames:        opt.Frames,
		PixelFormat:   "yuv420p",
		Scene:         opt.Scene,
		Seed:          opt.Seed,
		YUVFile:       yuvName,
		FrameBytes:    frameBytes,
		FileBytes:     frameBytes * int64(opt.Frames),
		SHA256:        hex.EncodeToString(h.Sum(nil)),
		TargetBPS:     1200000,
		GOP:           minInt(30, opt.Frames),
		SegmentFrames: minInt(60, opt.Frames),
	}
	if err := spec.Validate(); err != nil {
		return config.Spec{}, err
	}
	if err := writeManifest(filepath.Join(opt.OutDir, "manifest.json"), spec); err != nil {
		return config.Spec{}, err
	}
	return spec, nil
}

func writeManifest(path string, spec config.Spec) error {
	raw, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(path), ".manifest-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	ok := false
	defer func() {
		_ = tmp.Close()
		if !ok {
			_ = os.Remove(name)
		}
	}()
	if _, err := tmp.Write(raw); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	ok = true
	return nil
}

type palette struct {
	y0, y1 float64
	u0, v0 float64
	u1, v1 float64
}

func paletteFor(seed int64, scene int, frame int) palette {
	phase := float64(seed%17) * 0.13
	palettes := []palette{
		{y0: 52, y1: 176, u0: 112, v0: 142, u1: 168, v1: 104},
		{y0: 38, y1: 204, u0: 98, v0: 166, u1: 145, v1: 92},
		{y0: 68, y1: 226, u0: 128, v0: 118, u1: 94, v1: 164},
		{y0: 30, y1: 188, u0: 154, v0: 96, u1: 104, v1: 158},
	}
	idx := scene
	if scene >= 3 {
		idx = 2 + frame/90%2
	}
	p := palettes[(idx+int(seed%7))%len(palettes)]
	p.y0 += phase
	p.y1 -= phase
	return p
}

func renderFrame(opt Options, fi int) *yuv.Frame {
	w, h := opt.Width, opt.Height
	f, _ := yuv.NewFrame(w, h)
	scene := sceneAt(opt.Scene, fi, opt.Frames)
	p := paletteFor(opt.Seed, scene, fi)
	angle := float64(fi)*0.018 + float64(opt.Seed%11)*0.07
	grainAmp := 7
	switch scene {
	case 1:
		grainAmp = 13
	case 2:
		grainAmp = 9
	case 3:
		grainAmp = 8
	}
	rng := splitmix(uint64(opt.Seed) ^ uint64(fi+1)*0x9e3779b97f4a7c15)
	for y := 0; y < h; y++ {
		ny := (float64(y)/float64(h) - 0.5) * 2
		for x := 0; x < w; x++ {
			nx := (float64(x)/float64(w) - 0.5) * 2
			gradient := 0.5 + 0.25*math.Sin(2.2*nx+angle) + 0.15*ny
			luma := p.y0 + (p.y1-p.y0)*gradient
			faceWidth := 0.31 * (0.58 + 0.42*math.Abs(math.Cos(angle)))
			if math.Abs(nx) < faceWidth && math.Abs(ny) < 0.37 {
				shade := 0.52 + 0.48*(0.5+0.5*math.Cos(angle+nx*1.7))
				luma = p.y0 + (p.y1-p.y0)*(0.45+0.42*shade)
				if scene == 2 && opt.Scene == "mixed" {
					hx := nx - 0.18*math.Cos(angle)
					hy := ny + 0.12
					luma += 105 * math.Exp(-(hx*hx+hy*hy)*26)
				}
				luma += 16 * math.Sin((nx*4+ny*7)+angle*2)
			}
			luma += float64(int(rng.next()%uint64(2*grainAmp+1)) - grainAmp)
			f.Y[y*w+x] = clamp8(luma)
		}
	}
	for y := 0; y < h/2; y++ {
		ny := (float64(y)/float64(h/2) - 0.5) * 2
		for x := 0; x < w/2; x++ {
			nx := (float64(x)/float64(w/2) - 0.5) * 2
			t := 0.5 + 0.5*math.Sin(1.4*nx-0.9*ny+angle*0.4)
			u := p.u0 + (p.u1-p.u0)*t
			v := p.v0 + (p.v1-p.v0)*(1-t)
			uvAmp := grainAmp / 4
			u += float64(int(rng.next()%uint64(2*uvAmp+1)) - uvAmp)
			v += float64(int(rng.next()%uint64(2*uvAmp+1)) - uvAmp)
			idx := y*(w/2) + x
			f.U[idx] = clamp8(u)
			f.V[idx] = clamp8(v)
		}
	}
	return f
}

func sceneAt(name string, frame, frames int) int {
	if name != "mixed" {
		switch name {
		case "slow":
			return 0
		case "gradient", "grain":
			return 1
		case "cut":
			return 2 + frame/(max(90, frames/3))%2
		}
	}
	quarter := frames / 4
	if quarter < 1 {
		quarter = 1
	}
	scene := frame / quarter
	if scene > 3 {
		scene = 3
	}
	return scene
}

func clamp8(v float64) byte {
	v = math.Round(v)
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return byte(v)
}

type splitmixState struct{ x uint64 }

func splitmix(seed uint64) *splitmixState { return &splitmixState{x: seed} }

func (s *splitmixState) next() uint64 {
	s.x += 0x9e3779b97f4a7c15
	z := s.x
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// VerifyManifestSize checks the manifest against the file on disk.
func VerifyManifestSize(manifest config.Spec, baseDir string) error {
	path := filepath.Join(baseDir, manifest.YUVFile)
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Size() != manifest.FileBytes {
		return fmt.Errorf("manifest/bitstream size mismatch: file=%d manifest=%d", info.Size(), manifest.FileBytes)
	}
	if !strings.HasSuffix(manifest.YUVFile, ".yuv") {
		return fmt.Errorf("fixture: unexpected yuv basename")
	}
	return nil
}
