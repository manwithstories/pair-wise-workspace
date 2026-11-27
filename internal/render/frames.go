// Package render 把统一时间线的交错 PCM 离屏渲染为 JPEG 帧序列：
// 波形位图确定性生成，按目标码率二分搜索 JPEG 质量档，并产出逐帧哈希。
package render

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"

	"github.com/disintegration/imaging"

	"medpipe/internal/clock"
	"medpipe/internal/quality"
)

// FrameDims 是离屏帧的固定尺寸。
const (
	FrameWidth  = 320
	FrameHeight = 240
)

// Decode16Bit 把交错 16bit PCM 拆成每声道有符号样本。
func Decode16Bit(pcm []byte, channels int) ([][]int16, int, error) {
	if channels <= 0 || len(pcm)%(channels*2) != 0 {
		return nil, 0, clock.NewError(clock.CodeInvalidParam, -1, "render: PCM 长度与声道数不符")
	}
	frames := len(pcm) / (channels * 2)
	out := make([][]int16, channels)
	for c := range out {
		out[c] = make([]int16, frames)
	}
	for f := 0; f < frames; f++ {
		for c := 0; c < channels; c++ {
			off := (f*channels + c) * 2
			u := uint16(pcm[off]) | uint16(pcm[off+1])<<8
			out[c][f] = int16(u)
		}
	}
	return out, frames, nil
}

// WaveformImage 为指定帧区间生成确定性灰度波形位图。
// 不同声道以不同基线分层绘制，背景固定，保证同输入像素级可复现。
func WaveformImage(tracks [][]int16, start, end int64, frameIndex int) *image.Gray {
	img := image.NewGray(image.Rect(0, 0, FrameWidth, FrameHeight))
	// 深灰背景
	for i := range img.Pix {
		img.Pix[i] = 24
	}
	if len(tracks) == 0 || end <= start {
		return img
	}
	laneH := FrameHeight / len(tracks)
	for c, track := range tracks {
		base := laneH*c + laneH/2
		// 声道分隔线
		for x := 0; x < FrameWidth; x++ {
			img.SetGray(x, base, color.Gray{Y: 70})
		}
		span := int64(end - start)
		for x := 0; x < FrameWidth; x++ {
			idx := start + int64(x)*span/int64(FrameWidth)
			if idx >= int64(len(track)) {
				idx = int64(len(track)) - 1
			}
			mag := int32(track[idx]) * int32(laneH/2) / 32768
			y := base - int(mag)
			if y < 0 {
				y = 0
			}
			if y >= FrameHeight {
				y = FrameHeight - 1
			}
			img.SetGray(x, y, color.Gray{Y: 220})
		}
	}
	return img
}

// encodeAt 以给定 JPEG 质量编码，返回字节与可解码回的灰度图。
func encodeAt(img image.Image, q int) ([]byte, *image.Gray, error) {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: q}); err != nil {
		return nil, nil, clock.WrapError(clock.CodeInvalidParam, -1, err, "JPEG 编码失败")
	}
	dec, err := jpeg.Decode(bytes.NewReader(buf.Bytes()))
	if err != nil {
		return nil, nil, clock.WrapError(clock.CodeInvalidParam, -1, err, "JPEG 解码失败")
	}
	return buf.Bytes(), toGray(imaging.Grayscale(dec)), nil
}

// toGray 把 NRGBA 灰度图转为 image.Gray，便于按字节做 PSNR。
func toGray(src image.Image) *image.Gray {
	b := src.Bounds()
	dst := image.NewGray(b)
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			dst.SetGray(x, y, color.GrayModel.Convert(src.At(x, y)).(color.Gray))
		}
	}
	return dst
}

// SearchQuality 在 1..100 中二分搜索使单帧码率最接近目标的质量档。
// targetBytes<=0 时直接取高质量 90。返回质量、编码字节、参考图与压缩图。
func SearchQuality(img *image.Gray, targetBytes int) (int, []byte, *image.Gray, *image.Gray, error) {
	if targetBytes <= 0 {
		b, dec, err := encodeAt(img, 90)
		return 90, b, img, dec, err
	}
	lo, hi := 1, 100
	bestQ, bestB, bestDec := lo, []byte(nil), (*image.Gray)(nil)
	bestDiff := int(^uint(0) >> 1)
	for lo <= hi {
		mid := (lo + hi) / 2
		b, dec, err := encodeAt(img, mid)
		if err != nil {
			return 0, nil, nil, nil, err
		}
		diff := abs(len(b) - targetBytes)
		if diff < bestDiff {
			bestDiff, bestQ, bestB, bestDec = diff, mid, b, dec
		}
		if len(b) < targetBytes {
			lo = mid + 1
		} else {
			hi = mid - 1
		}
	}
	return bestQ, bestB, img, bestDec, nil
}

// RenderFrame 渲染并落盘一帧，附带哈希与对参考位图的 PSNR。
func RenderFrame(dir string, idx int64, img, compressed *image.Gray, jpegBytes []byte) (quality.FrameMetric, error) {
	name := frameName(idx)
	full, err := clockSafeJoin(dir, name)
	if err != nil {
		return quality.FrameMetric{}, err
	}
	if err := os.WriteFile(full, jpegBytes, 0o644); err != nil {
		return quality.FrameMetric{}, clock.WrapError(clock.CodeInvalidParam, idx, err, "帧写入失败")
	}
	psnr, err := quality.PSNR(img.Pix, compressed.Pix)
	if err != nil {
		return quality.FrameMetric{}, err
	}
	return quality.FrameMetric{
		Index:  idx,
		File:   name,
		SHA256: quality.HashBytes(jpegBytes),
		Width:  FrameWidth,
		Height: FrameHeight,
		Bytes:  len(jpegBytes),
		PSNR:   psnr,
	}, nil
}

func frameName(idx int64) string {
	return filepath.Join("frame_" + pad6(idx) + ".jpg")
}

func pad6(v int64) string {
	digits := []byte("000000")
	s := []byte(itoa(v))
	copy(digits[len(digits)-len(s):], s)
	return string(digits)
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:])
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
