package abr

import (
	"math"

	"avpipe/internal/media"
)

// YMSE computes the mean squared error of the luma (Y) plane between two
// equally sized I420 frames. Width/height are taken from src.
func YMSE(src, rec *media.Frame) float64 {
	if src.Width != rec.Width || src.Height != rec.Height {
		return math.NaN()
	}
	n := src.Width * src.Height
	if n == 0 || len(src.Y) < n || len(rec.Y) < n {
		return math.NaN()
	}
	var acc uint64
	for i := 0; i < n; i++ {
		d := int(src.Y[i]) - int(rec.Y[i])
		acc += uint64(d * d)
	}
	return float64(acc) / float64(n)
}

// PSNRY returns the luma peak signal-to-noise ratio in dB for an 8-bit
// plane. Perfectly identical frames return +Inf; callers format that case.
func PSNRY(mse float64) float64 {
	if mse <= 0 {
		return math.Inf(1)
	}
	return 10 * math.Log10((255.0*255.0)/mse)
}

// FramePSNR is the convenience wrapper used by the pipeline.
func FramePSNR(src, rec *media.Frame) float64 {
	return PSNRY(YMSE(src, rec))
}

// DownscaleI420 returns a new frame at dstW x dstH using bilinear filtering
// per plane. It is the pure-Go reference used to score encoded segments.
func DownscaleI420(src *media.Frame, dstW, dstH int) *media.Frame {
	out := &media.Frame{Seq: src.Seq, PTS: src.PTS, Width: dstW, Height: dstH, Gapped: src.Gapped}
	out.Y = downscalePlane(src.Y, src.Width, src.Height, dstW, dstH)
	cw, ch := src.Width/2, src.Height/2
	out.Cb = downscalePlane(src.Cb, cw, ch, dstW/2, dstH/2)
	out.Cr = downscalePlane(src.Cr, cw, ch, dstW/2, dstH/2)
	return out
}

// downscalePlane performs bilinear resampling of one byte plane.
func downscalePlane(src []byte, srcW, srcH, dstW, dstH int) []byte {
	if srcW == dstW && srcH == dstH {
		return append([]byte(nil), src...)
	}
	dst := make([]byte, dstW*dstH)
	for y := 0; y < dstH; y++ {
		fy := float64(y) * float64(srcH-1) / float64(dstH-1)
		y0 := int(fy)
		y1 := y0 + 1
		if y1 >= srcH {
			y1 = srcH - 1
		}
		wy := fy - float64(y0)
		for x := 0; x < dstW; x++ {
			fx := float64(x) * float64(srcW-1) / float64(dstW-1)
			x0 := int(fx)
			x1 := x0 + 1
			if x1 >= srcW {
				x1 = srcW - 1
			}
			wx := fx - float64(x0)
			v00 := float64(src[y0*srcW+x0])
			v01 := float64(src[y0*srcW+x1])
			v10 := float64(src[y1*srcW+x0])
			v11 := float64(src[y1*srcW+x1])
			top := v00*(1-wx) + v01*wx
			bot := v10*(1-wx) + v11*wx
			dst[y*dstW+x] = byte(top*(1-wy) + bot*wy + 0.5)
		}
	}
	return dst
}

// MeanPSNR averages per-segment dB values. Infinite values (lossless frames)
// are capped at 60 dB so they do not dominate the mean.
func MeanPSNR(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	var sum float64
	for _, v := range values {
		if math.IsInf(v, 1) || v > 60 {
			v = 60
		}
		sum += v
	}
	return sum / float64(len(values))
}

// AVDriftMS computes the audio-vs-video offset in milliseconds for one
// segment, given the first presentation timestamps on the shared clock.
// Positive means audio lags video.
func AVDriftMS(videoPTS, audioPTS uint64) float64 {
	d := media.ClockDiff(audioPTS, videoPTS)
	return float64(d) * 1000.0 / float64(media.ClockRate)
}

// QualityStats aggregates per-segment quality measurements for a run.
type QualityStats struct {
	PSNRValues    []float64
	DriftValuesMS []float64
	MaxDriftMS    float64
}

// Add records one segment's luma PSNR and AV drift.
func (q *QualityStats) Add(psnrDB, driftMS float64) {
	q.PSNRValues = append(q.PSNRValues, psnrDB)
	q.DriftValuesMS = append(q.DriftValuesMS, driftMS)
	if math.Abs(driftMS) > math.Abs(q.MaxDriftMS) {
		q.MaxDriftMS = driftMS
	}
}

// MeanPSNR returns the run-level average luma PSNR.
func (q *QualityStats) MeanPSNR() float64 { return MeanPSNR(q.PSNRValues) }
