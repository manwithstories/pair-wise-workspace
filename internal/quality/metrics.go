// Package quality 汇总离屏导出的质量证据：逐帧哈希、PSNR、削波、
// 缺失样本、时间对齐误差与目标码率偏差。
package quality

import (
	"crypto/sha256"
	"encoding/hex"
	"math"

	"medpipe/internal/clock"
)

// FrameMetric 是单帧的质量记录。
type FrameMetric struct {
	Index     int64  `json:"index"`
	File      string `json:"file"`
	SHA256    string `json:"sha256"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
	Bytes     int    `json:"bytes"`
	PSNR      float64 `json:"psnr_db"`
	ClipCount int64   `json:"clipped_samples"`
}

// Summary 是整条时间线的汇总证据，字段名与终端验收输出对齐。
type Summary struct {
	Frames            int64   `json:"frames"`
	TargetBPS         int64   `json:"target_bps"`
	ActualBPS         int64   `json:"actual_bps"`
	DeviationPct      float64 `json:"deviation_pct"`
	PSNRMin           float64 `json:"psnr_db_min"`
	PSNRMean          float64 `json:"psnr_db_mean"`
	ClippedSamples    int64   `json:"clipped_samples"`
	MissingSamples    int64   `json:"missing_samples"`
	MaxErrorSamples   int64   `json:"manifest_max_error_samples"`
	CompensatedFrames int64   `json:"compensated_frames"`
	FrameSHA256       string  `json:"frame_sha256"` // 全部帧哈希的链式摘要
}

// HashBytes 返回任意字节块的十六进制 SHA-256。
func HashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ChainHash 对逐帧哈希做链式摘要，保证帧序列顺序也进入证据。
func ChainHash(hashes []string) string {
	h := sha256.New()
	for _, s := range hashes {
		h.Write([]byte(s))
		h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// PSNR 以 8bit 灰度峰值 255 计算两图的峰值信噪比；完全一致返回 +Inf。
func PSNR(ref, got []byte) (float64, error) {
	if len(ref) != len(got) || len(ref) == 0 {
		return 0, clock.NewError(clock.CodeInvalidParam, -1, "quality: PSNR 输入长度不一致或为空")
	}
	var mse float64
	for i := range ref {
		d := float64(ref[i]) - float64(got[i])
		mse += d * d
	}
	mse /= float64(len(ref))
	if mse == 0 {
		return math.Inf(1), nil
	}
	return 10 * math.Log10(255*255/mse), nil
}

// Bitrate 按帧字节数与帧率反推实际码率（bit/s）。
func Bitrate(totalFrameBytes, frames int64, fpsNum, fpsDen int64) int64 {
	if frames <= 0 || fpsNum <= 0 {
		return 0
	}
	duration := float64(frames) * float64(fpsDen) / float64(fpsNum)
	if duration <= 0 {
		return 0
	}
	return int64(math.Round(float64(totalFrameBytes) * 8 / duration))
}

// DeviationPct 返回实际码率相对目标码率的百分比偏差（带符号）。
func DeviationPct(target, actual int64) float64 {
	if target <= 0 {
		return 0
	}
	return (float64(actual-target) / float64(target)) * 100
}

// Finalize 汇总逐帧指标与音频侧统计，计算链式哈希与码率偏差。
func Finalize(frames []FrameMetric, targetBPS int64, fpsNum, fpsDen int64,
	clipped, missing, maxAlign, compensated int64) Summary {
	hashes := make([]string, len(frames))
	var totalBytes int64
	var psnrSum float64
	psnrMin := math.Inf(1)
	for i, fm := range frames {
		hashes[i] = fm.SHA256
		totalBytes += int64(fm.Bytes)
		psnrSum += fm.PSNR
		if fm.PSNR < psnrMin {
			psnrMin = fm.PSNR
		}
	}
	actual := Bitrate(totalBytes, int64(len(frames)), fpsNum, fpsDen)
	s := Summary{
		Frames:            int64(len(frames)),
		TargetBPS:         targetBPS,
		ActualBPS:         actual,
		DeviationPct:      DeviationPct(targetBPS, actual),
		PSNRMin:           psnrMin,
		PSNRMean:          psnrSum / float64(len(frames)),
		ClippedSamples:    clipped,
		MissingSamples:    missing,
		MaxErrorSamples:   maxAlign,
		CompensatedFrames: compensated,
		FrameSHA256:       ChainHash(hashes),
	}
	if len(frames) == 0 {
		s.PSNRMin, s.PSNRMean = 0, 0
	}
	return s
}
