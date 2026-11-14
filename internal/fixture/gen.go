// Package fixture 生成确定性的 YUV420 帧序列，用作编码测试夹具。
package fixture

import (
	"fmt"
	"math"
	"math/rand"
	"os"
)

// Config 描述一支夹具序列的生成参数。
type Config struct {
	Scene  string // gradient | texture | motion | noise
	Seed   int64
	Width  int
	Height int
	Frames int
}

// Frame 是一帧 YUV420  planar 数据。
type Frame struct {
	W, H    int
	Y, U, V []byte
}

// NewFrame 分配一帧空白 YUV420。
func NewFrame(w, h int) *Frame {
	return &Frame{
		W: w, H: h,
		Y: make([]byte, w*h),
		U: make([]byte, w*h/4),
		V: make([]byte, w*h/4),
	}
}

// FrameSize 返回单帧字节数（YUV420）。
func FrameSize(w, h int) int { return w * h * 3 / 2 }

// Validate 校验生成参数。
func (c Config) Validate() error {
	switch c.Scene {
	case "gradient", "texture", "motion", "noise":
	default:
		return fmt.Errorf("未知场景 %q（可选 gradient|texture|motion|noise）", c.Scene)
	}
	if c.Width <= 0 || c.Height <= 0 || c.Width%8 != 0 || c.Height%8 != 0 {
		return fmt.Errorf("分辨率须为正且宽高均为 8 的倍数，当前 %dx%d", c.Width, c.Height)
	}
	if c.Frames <= 0 {
		return fmt.Errorf("帧数须为正，当前 %d", c.Frames)
	}
	return nil
}

// Generate 按场景与种子生成确定性帧序列。
func Generate(cfg Config) ([]*Frame, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	rng := rand.New(rand.NewSource(cfg.Seed))
	frames := make([]*Frame, cfg.Frames)
	for t := range frames {
		f := NewFrame(cfg.Width, cfg.Height)
		switch cfg.Scene {
		case "gradient":
			paintGradient(f, t)
		case "texture":
			paintTexture(f, t, rng)
		case "motion":
			paintMotion(f, t, rng)
		case "noise":
			paintNoise(f, t, rng)
		}
		frames[t] = f
	}
	return frames, nil
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

// paintGradient 绘制随时间缓慢平移的斜向灰度渐变。
func paintGradient(f *Frame, t int) {
	for y := 0; y < f.H; y++ {
		for x := 0; x < f.W; x++ {
			v := math.Mod(float64(x+y/2+t*3), 256)
			f.Y[y*f.W+x] = byte(v)
		}
	}
	flatChroma(f)
}

// paintTexture 绘制种子决定的静态纹理，叠加缓慢的亮度呼吸。
func paintTexture(f *Frame, t int, rng *rand.Rand) {
	if t == 0 {
		textureCache = make([]byte, f.W*f.H)
		for i := range textureCache {
			textureCache[i] = byte(rng.Intn(256))
		}
	}
	breathe := 8 * math.Sin(float64(t)*0.1)
	for i, p := range textureCache[:f.W*f.H] {
		f.Y[i] = clamp8(float64(p)*0.6 + 50 + breathe)
	}
	flatChroma(f)
}

var textureCache []byte

// paintMotion 绘制渐变背景上运动的方块与圆，叠加轻微噪声。
func paintMotion(f *Frame, t int, rng *rand.Rand) {
	for y := 0; y < f.H; y++ {
		for x := 0; x < f.W; x++ {
			f.Y[y*f.W+x] = byte((x + y) * 180 / (f.W + f.H))
		}
	}
	// 弹跳方块
	rw, rh := f.W/6, f.H/6
	rx := bounce(t*4, f.W-rw)
	ry := bounce(t*3, f.H-rh)
	for y := ry; y < ry+rh; y++ {
		for x := rx; x < rx+rw; x++ {
			f.Y[y*f.W+x] = 220
		}
	}
	// 运动圆
	cr := f.H / 10
	cx := bounce(t*2+cr, f.W-2*cr) + cr
	cy := bounce(t*5+cr, f.H-2*cr) + cr
	for y := cy - cr; y < cy+cr; y++ {
		for x := cx - cr; x < cx+cr; x++ {
			if (x-cx)*(x-cx)+(y-cy)*(y-cy) <= cr*cr {
				f.Y[y*f.W+x] = 30
			}
		}
	}
	// 轻微颗粒噪声，避免内容过于平坦
	for i := range f.Y {
		f.Y[i] = clamp8(float64(f.Y[i]) + float64(rng.Intn(7)-3))
	}
	flatChroma(f)
}

// bounce 返回 0..limit 之间往返的位置。
func bounce(pos, limit int) int {
	if limit <= 0 {
		return 0
	}
	period := 2 * limit
	p := pos % period
	if p < 0 {
		p += period
	}
	if p > limit {
		p = period - p
	}
	return p
}

// paintNoise 绘制中灰底上的逐帧随机噪声。
func paintNoise(f *Frame, t int, rng *rand.Rand) {
	for i := range f.Y {
		f.Y[i] = clamp8(120 + float64(rng.Intn(51)-25))
	}
	flatChroma(f)
}

func flatChroma(f *Frame) {
	for i := range f.U {
		f.U[i] = 128
		f.V[i] = 128
	}
}

// Bytes 把一帧序列化为 YUV420 字节流。
func (f *Frame) Bytes() []byte {
	out := make([]byte, 0, FrameSize(f.W, f.H))
	out = append(out, f.Y...)
	out = append(out, f.U...)
	out = append(out, f.V...)
	return out
}

// WriteFile 把帧序列写入原始 YUV 文件。
func WriteFile(path string, frames []*Frame) error {
	fp, err := os.Create(path)
	if err != nil {
		return err
	}
	defer fp.Close()
	for _, f := range frames {
		if _, err := fp.Write(f.Bytes()); err != nil {
			return err
		}
	}
	return nil
}

// ReadFrames 从字节流切出 w×h 的 YUV420 帧序列。
func ReadFrames(data []byte, w, h int) ([]*Frame, error) {
	fs := FrameSize(w, h)
	if len(data) == 0 || len(data)%fs != 0 {
		return nil, fmt.Errorf("文件大小 %d 不是单帧 %d 字节的整数倍", len(data), fs)
	}
	n := len(data) / fs
	frames := make([]*Frame, n)
	yN, cN := w*h, w*h/4
	for i := 0; i < n; i++ {
		base := i * fs
		f := NewFrame(w, h)
		copy(f.Y, data[base:base+yN])
		copy(f.U, data[base+yN:base+yN+cN])
		copy(f.V, data[base+yN+cN:base+fs])
		frames[i] = f
	}
	return frames, nil
}
