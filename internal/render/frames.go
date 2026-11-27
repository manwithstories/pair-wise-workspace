// Package render 把时间线样本渲染为可复核的离屏 PNG 帧序列。
// 不打开任何窗口、不依赖浏览器或设备：仅用 image/imaging 在内存中
// 逐像素合成确定性图案，写盘后对原始像素计算 SHA256，供独立核验复算。
package render

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"sort"

	"github.com/disintegration/imaging"

	"bcast/internal/fault"
	"bcast/internal/model"
)

// Frame 尺寸与采样步长硬上限，控制内存与产物规模。
const (
	FrameW = 160
	FrameH = 90
)

// FrameSpec 描述待导出的一帧：在主时钟上的时间与用于合成的确定性种子。
type FrameSpec struct {
	Index  int
	TimeUS int64
	Seed   uint64
	Track  uint8
}

// PlanFrames 按 reviewStep（微秒）在时间轴上等间隔规划 120 帧以内的帧序列，
// 完全由输入时长决定，不依赖系统时间，保证可复现。
func PlanFrames(durationUS, reviewStepUS int64, maxFrames int) []FrameSpec {
	if reviewStepUS <= 0 || durationUS <= 0 {
		return nil
	}
	var specs []FrameSpec
	idx := 0
	for t := int64(0); t < durationUS && len(specs) < maxFrames; t += reviewStepUS {
		specs = append(specs, FrameSpec{
			Index: idx, TimeUS: t, Seed: uint64(t), Track: 0,
		})
		idx++
	}
	return specs
}

// Draw 在内存中合成一帧确定性图案：底色随帧索引渐变，叠加由种子决定的
// 网格与时间刻度条，三路 VFR 以三种通道强度区分。像素完全确定、无随机源。
func Draw(spec FrameSpec, sample model.Sample) *image.NRGBA {
	img := imaging.New(FrameW, FrameH, color.NRGBA{
		R: uint8(spec.Index * 7), G: uint8(spec.Index * 13), B: uint8(spec.Index * 29), A: 255,
	})
	bounds := img.Bounds()
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			// 网格：每 16 像素一条高亮线，亮度由种子与坐标确定。
			grid := uint8(0)
			if x%16 == 0 || y%16 == 0 {
				grid = 60
			}
			// 时间刻度条：底部 8 像素按帧时间取相位。
			bar := uint8(0)
			if y >= FrameH-8 {
				bar = uint8((spec.TimeUS/1000 + int64(x)*7) & 0xFF)
			}
			base := img.NRGBAAt(x, y)
			base.R = sat(int(base.R) + int(grid) + int(sample.TrackID*30))
			base.G = sat(int(base.G) + int(grid) + int(byte(spec.Seed>>24)))
			base.B = sat(int(base.B) + int(grid) + int(bar))
			img.SetNRGBA(x, y, base)
		}
	}
	// 左上角写入帧序号的确定性色块，方便人工肉眼复核顺序。
	for y := 0; y < 6; y++ {
		for x := 0; x < 6; x++ {
			v := uint8((spec.Index >> uint(x+y)) & 1)
			img.SetNRGBA(x, y, color.NRGBA{R: 255 * v, G: 255 * v, B: 255, A: 255})
		}
	}
	return img
}

func sat(v int) uint8 {
	if v > 255 {
		return 255
	}
	if v < 0 {
		return 0
	}
	return uint8(v)
}

// PixelHash 对图像原始像素（不含 PNG 编码）计算 SHA256，作为像素级复核哈希。
func PixelHash(img *image.NRGBA) string {
	h := sha256.New()
	var buf [4]byte
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			c := img.NRGBAAt(x, y)
			buf[0], buf[1], buf[2], buf[3] = c.R, c.G, c.B, c.A
			h.Write(buf[:])
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Export 把帧序列写入 outDir/frames/frame_%04d.png，并返回带像素哈希的引用。
// 写盘采用“先写临时文件再 rename”，避免半成品帧被复核程序读到。
func Export(outDir string, specs []FrameSpec, sampleAt func(FrameSpec) model.Sample) ([]model.FrameRef, error) {
	frameDir := filepath.Join(outDir, "frames")
	if err := os.MkdirAll(frameDir, 0o755); err != nil {
		return nil, err
	}
	refs := make([]model.FrameRef, 0, len(specs))
	for _, spec := range specs {
		img := Draw(spec, sampleAt(spec))
		hash := PixelHash(img)
		name := frameName(spec.Index)
		final := filepath.Join(frameDir, name)
		if err := writePNGAtomic(final, img); err != nil {
			return nil, err
		}
		if err := WriteBMPSidecar(final, img); err != nil {
			return nil, err
		}
		refs = append(refs, model.FrameRef{
			Index: spec.Index, TimeUS: spec.TimeUS, Path: final, Sha256: hash,
		})
	}
	// 引用按索引有序，保证清单确定性。
	sort.Slice(refs, func(i, j int) bool { return refs[i].Index < refs[j].Index })
	return refs, nil
}

func frameName(index int) string {
	return fmt.Sprintf("frame_%04d.png", index)
}

func writePNGAtomic(path string, img image.Image) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".frame-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if err := png.Encode(tmp, img); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

// RehashPNG 独立复算已落盘 PNG 的像素哈希：解码后逐像素重算，
// 与导出时记录的哈希比对可证明帧序列未被篡改。
func RehashPNG(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		return "", fault.New(fault.ErrInvalidInput, "render", 0, 0, 0, 0, err.Error())
	}
	nrgba, ok := img.(*image.NRGBA)
	if !ok {
		nrgba = imaging.Clone(img)
	}
	return PixelHash(nrgba), nil
}
