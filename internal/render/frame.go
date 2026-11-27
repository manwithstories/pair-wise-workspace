// Package render 按时间点离屏导出 JPEG 帧序列，
// 输出逐帧 SHA-256 与像素尺寸。
package render

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"sort"

	"github.com/disintegration/imaging"
)

// FrameInput 一帧的渲染输入。
type FrameInput struct {
	Name   string // 输出文件名（不含扩展名）
	Source []byte // 源 JPEG 帧；nil 或损坏时生成确定性占位图
	Seed   int    // 占位图种子
}

// Frame 一帧的渲染产物。
type Frame struct {
	Path   string
	SHA256 string
	Width  int
	Height int
}

// MakeFrameImage 生成确定性测试图样（夹具与占位共用）。
func MakeFrameImage(seed, w, h int) *image.NRGBA {
	img := imaging.New(w, h, color.NRGBA{0, 0, 0, 255})
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			r := uint8((x*3 + seed*17) % 256)
			g := uint8((y*5 + seed*31) % 256)
			b := uint8(((x+y)/8 + seed*7) % 256)
			img.SetNRGBA(x, y, color.NRGBA{r, g, b, 255})
		}
	}
	return img
}

// EncodeJPEG 以指定质量编码 JPEG。
func EncodeJPEG(img image.Image, quality int) ([]byte, error) {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// RenderSequence 离屏渲染帧序列，返回逐帧 SHA-256、尺寸与整体摘要。
func RenderSequence(dir string, inputs []FrameInput, quality int) ([]Frame, string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, "", err
	}
	var frames []Frame
	for _, in := range inputs {
		img, err := imaging.Decode(bytes.NewReader(in.Source))
		if err != nil {
			// 源损坏：确定性占位图，保证可复核
			img = MakeFrameImage(in.Seed, 160, 90)
		}
		data, err := EncodeJPEG(img, quality)
		if err != nil {
			return nil, "", fmt.Errorf("encode %s: %w", in.Name, err)
		}
		path := filepath.Join(dir, in.Name+".jpg")
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return nil, "", err
		}
		sum := sha256.Sum256(data)
		frames = append(frames, Frame{
			Path:   path,
			SHA256: hex.EncodeToString(sum[:]),
			Width:  img.Bounds().Dx(),
			Height: img.Bounds().Dy(),
		})
	}
	return frames, DigestFrames(frames), nil
}

// DigestFrames 汇总逐帧摘要为整体 SHA-256（按路径排序）。
func DigestFrames(frames []Frame) string {
	sorted := make([]Frame, len(frames))
	copy(sorted, frames)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })
	h := sha256.New()
	for _, f := range sorted {
		h.Write([]byte(f.SHA256))
	}
	return hex.EncodeToString(h.Sum(nil))
}
