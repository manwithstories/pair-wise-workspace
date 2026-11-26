// Package render 用 imaging 离屏把对齐后的槽位合成为带轨道与时间戳
// 标注的 PNG 帧序列，并产出确定性的帧清单。
package render

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"

	"github.com/disintegration/imaging"

	"tsproof/internal/clock"
	"tsproof/internal/mixer"
)

// 渲染几何与标注约束。
const (
	CanvasW    = 320
	CanvasH    = 240
	PanelW     = 160 // 视频缩略图面板宽
	labelStrip = 24  // 底部时间戳标注条高度
)

// 离屏合成用的确定性调色板（无字体依赖，用色块/条纹表达轨道与状态）。
var (
	colorBG      = color.NRGBA{R: 18, G: 22, B: 30, A: 255}
	colorStrip   = color.NRGBA{R: 12, G: 14, B: 20, A: 255}
	colorOK      = color.NRGBA{R: 40, G: 180, B: 90, A: 255}
	colorWarn    = color.NRGBA{R: 220, G: 160, B: 40, A: 255}
	colorAudio   = color.NRGBA{R: 60, G: 120, B: 220, A: 255}
)

// ManifestEntry 是单帧清单条目，随帧序列一并导出供复核。
type ManifestEntry struct {
	Seq       int    `json:"seq"`
	File      string `json:"file"`
	PTS27     int64  `json:"pts27"`
	PTSSecs   string `json:"pts_secs"`
	Key       bool   `json:"key"`
	Untrusted bool   `json:"untrusted"`
	AudioN    int    `json:"audio_blocks"`
	SHA256    string `json:"sha256"`
}

// Renderer 把槽位渲染到指定输出目录。
type Renderer struct {
	outDir string
}

// New 创建渲染器，目录不存在时创建。
func New(outDir string) (*Renderer, error) {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, fmt.Errorf("render: mkdir: %w", err)
	}
	return &Renderer{outDir: outDir}, nil
}

// RenderSlot 合成单个槽位为 PNG，返回帧字节（不直接落盘，便于测试与哈希）。
// 布局：上半为视频缩略图，右侧为音频电平块，底部为时间戳/状态标注条。
func (r *Renderer) RenderSlot(slot mixer.Slot, seq int) ([]byte, ManifestEntry, error) {
	canvas := image.NewNRGBA(image.Rect(0, 0, CanvasW, CanvasH))
	fill(canvas, colorBG)

	// 视频面板：解码 JPEG 块并缩放贴入左侧。
	frameImg, err := jpeg.Decode(bytes.NewReader(slot.Frame.Payload))
	if err != nil {
		return nil, ManifestEntry{}, fmt.Errorf("render: jpeg decode: %w", err)
	}
	thumb := imaging.Fit(frameImg, PanelW, CanvasH-labelStrip, imaging.Lanczos)
	tb := thumb.Bounds()
	drawAt(canvas, thumb, (PanelW-tb.Dx())/2, (CanvasH-labelStrip-tb.Dy())/2)

	// 右侧音频面板：每个音频块画一条确定高度的电平条。
	drawAudioPanel(canvas, slot)

	// 底部标注条：关键帧=绿色，不可信=琥珀色；并画时间码刻度。
	statusColor := colorOK
	if slot.Untrusted {
		statusColor = colorWarn
	}
	for y := CanvasH - labelStrip; y < CanvasH; y++ {
		for x := 0; x < CanvasW; x++ {
			canvas.SetNRGBA(x, y, colorStrip)
		}
	}
	// 左侧 8 像素状态色块。
	for y := CanvasH - labelStrip + 4; y < CanvasH - 4; y++ {
		for x := 4; x < 12; x++ {
			canvas.SetNRGBA(x, y, statusColor)
		}
	}
	// 时间码刻度：按 PTS 毫秒画等宽刻度纹，保证无字体也能肉眼核对时间。
	drawTickScale(canvas, slot.At, slot.FrameKind)

	var buf bytes.Buffer
	if err := imaging.Encode(&buf, canvas, imaging.PNG); err != nil {
		return nil, ManifestEntry{}, fmt.Errorf("render: png encode: %w", err)
	}
	png := buf.Bytes()
	pts := clock.Tick(slot.Frame.PTS27)
	entry := ManifestEntry{
		Seq:       seq,
		File:      frameName(seq),
		PTS27:     slot.Frame.PTS27,
		PTSSecs:   fmt.Sprintf("%.6f", pts.Seconds()),
		Key:       slot.Frame.Key,
		Untrusted: slot.Untrusted,
		AudioN:    len(slot.Audio),
		SHA256:    sha256Hex(png),
	}
	return png, entry, nil
}

// WriteFrame 把 PNG 字节写到输出目录。
func (r *Renderer) WriteFrame(seq int, png []byte) error {
	path := filepath.Join(r.outDir, frameName(seq))
	if err := os.WriteFile(path, png, 0o644); err != nil {
		return fmt.Errorf("render: write %s: %w", path, err)
	}
	return nil
}

func frameName(seq int) string {
	return fmt.Sprintf("frame_%04d.png", seq)
}

func fill(img *image.NRGBA, c color.NRGBA) {
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			img.SetNRGBA(x, y, c)
		}
	}
}

func drawAt(dst *image.NRGBA, src image.Image, ox, oy int) {
	b := src.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			dst.Set(ox+x, oy+y, src.At(x, y))
		}
	}
}

func drawAudioPanel(canvas *image.NRGBA, slot mixer.Slot) {
	baseX := PanelW + 8
	maxBars := 8
	for i := 0; i < maxBars; i++ {
		c := color.NRGBA{R: 40, G: 48, B: 60, A: 255}
		h := 8
		if i < len(slot.Audio) {
			c = colorAudio
			// 用样本数确定条高，确定性且可复核。
			h = 12 + (slot.Audio[i].Samples%40)
		}
		x := baseX + i*16
		for y := CanvasH - labelStrip - 8; y > CanvasH-labelStrip-8-h; y-- {
			for dx := 0; dx < 10; dx++ {
				canvas.SetNRGBA(x+dx, y, c)
			}
		}
	}
}

func drawTickScale(canvas *image.NRGBA, at clock.Tick, kind clock.FrameKind) {
	// 每 90k tick（1/300s）画一条 2px 纹，关键帧纹更长。
	unit := clock.Tick(90_000)
	ticks := int((at / unit) % 64)
	for i := 0; i < ticks; i++ {
		x := 20 + i*4
		if x >= CanvasW-2 {
			break
		}
		h := 6
		if kind == clock.FrameKey && i%8 == 0 {
			h = 12
		}
		for y := CanvasH - h - 4; y < CanvasH-4; y++ {
			canvas.SetNRGBA(x, y, color.NRGBA{R: 200, G: 210, B: 220, A: 255})
			canvas.SetNRGBA(x+1, y, color.NRGBA{R: 200, G: 210, B: 220, A: 255})
		}
	}
}
