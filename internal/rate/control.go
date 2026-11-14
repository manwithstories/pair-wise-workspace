// Package rate implements GOP-level bitrate control with a VBV buffer model,
// cold-start probing, and online log(bits)-versus-QP regression.
package rate

import (
	"fmt"
	"math"

	"gonum.org/v1/gonum/stat"
)

const maxSamples = 64

type Config struct {
	TargetBPS    int64
	FPS          int
	GOP          int
	BufferBits   float64
	InitialLevel float64
	PixelCount   int
}

// BufferState is serialized into every GOP checkpoint.
type BufferState struct {
	LevelBits    float64 `json:"level_bits"`
	CapacityBits float64 `json:"capacity_bits"`
	CumTarget    float64 `json:"cum_target_bits"`
	CumActual    float64 `json:"cum_actual_bits"`
	Frames       int     `json:"frames"`
}

type sample struct {
	QP   float64
	Bits float64
}

type Controller struct {
	targetPerFrame float64
	capacity       float64
	level          float64
	cumTarget      float64
	cumActual      float64
	frames         int
	samples        []sample
	lastQP         int
	lowQP          int
	highQP         int
	lastComplexity float64
	sceneResets    int
	pixelCount     int
}

func New(cfg Config) (*Controller, error) {
	if cfg.TargetBPS <= 0 || cfg.FPS <= 0 || cfg.GOP <= 0 {
		return nil, fmt.Errorf("rate: invalid controller config")
	}
	capacity := cfg.BufferBits
	if capacity <= 0 {
		capacity = float64(cfg.TargetBPS)
	}
	level := cfg.InitialLevel
	if level <= 0 {
		level = capacity / 2
	}
	if level > capacity {
		level = capacity
	}
	pixels := cfg.PixelCount
	if pixels <= 0 {
		pixels = 320 * 180
	}
	return &Controller{
		targetPerFrame: float64(cfg.TargetBPS) / float64(cfg.FPS),
		capacity:       capacity,
		level:          level,
		lastQP:         28,
		lowQP:          1,
		highQP:         51,
		pixelCount:     pixels,
	}, nil
}

// BudgetBits incorporates both the nominal GOP budget and a bounded share of
// the cumulative buffer deficit.
func (c *Controller) BudgetBits(frames int) float64 {
	if frames <= 0 {
		return 0
	}
	base := c.targetPerFrame * float64(frames)
	deficit := c.cumTarget - c.cumActual
	correction := 0.65 * deficit
	limit := 0.5 * base
	if limit < 1 {
		limit = 1
	}
	correction = math.Max(-limit, math.Min(limit, correction))
	return math.Max(128, base+correction)
}

// QPForGOP returns the starting QP. With no history it uses a bounded
// complexity-aware cold-start estimate; otherwise it inverts the online fit.
func (c *Controller) QPForGOP(frames int, complexity float64) int {
	target := c.BudgetBits(frames)
	if len(c.samples) >= 2 {
		if qp, ok := c.regressionQP(target); ok {
			return c.clampAndProbe(qp)
		}
	}
	activity := complexity
	if activity <= 0 {
		activity = 1
	}
	bpp := target / (float64(frames) * float64(c.pixelCount))
	if bpp <= 0 {
		bpp = 1
	}
	qp := 34.0 - 4.0*math.Log2(bpp/0.5) + 1.5*math.Log2(math.Max(activity, 1e-3)/0.1)
	return c.clampAndProbe(int(math.Round(qp)))
}

// AdjustQP predicts a new QP after a trial encode. The slope is six QP per
// octave of rate, then constrained to a useful local correction.
func (c *Controller) AdjustQP(targetBits, actualBits float64, qp int) int {
	if actualBits <= 0 || targetBits <= 0 {
		return clampQP(qp)
	}
	ratio := targetBits / actualBits
	delta := -6.0 * math.Log2(ratio)
	if delta > 8 {
		delta = 8
	} else if delta < -8 {
		delta = -8
	}
	return clampQP(qp + int(math.Round(delta)))
}

// Observe records the final GOP size and tightens the cold-start bracket.
func (c *Controller) Observe(qp int, bits int64, frames int, complexity float64) {
	qp = clampQP(qp)
	target := c.targetPerFrame * float64(frames)
	c.cumTarget += target
	c.cumActual += float64(bits)
	c.frames += frames
	c.lastQP = qp
	c.lastComplexity = complexity
	// A full VBV sees the GOP target enter and the actual bits leave.
	c.level += target - float64(bits)
	c.level = math.Max(0, math.Min(c.capacity, c.level))
	if bits > 0 {
		c.samples = append(c.samples, sample{QP: float64(qp), Bits: math.Log(math.Max(1, float64(bits)))})
		if len(c.samples) > maxSamples {
			c.samples = c.samples[len(c.samples)-maxSamples:]
		}
	}
	if float64(bits) > target*1.08 {
		c.lowQP = max(c.lowQP, qp+1)
	}
	if float64(bits) < target*0.92 {
		c.highQP = min(c.highQP, qp-1)
	}
	if c.lowQP > c.highQP {
		c.lowQP, c.highQP = clampQP(qp-2), clampQP(qp+2)
	}
}

// ResetScene discards local probing assumptions after a hard cut, while
// retaining the global bitrate history.
func (c *Controller) ResetScene(qp int) {
	c.sceneResets++
	c.lowQP = clampQP(qp - 3)
	c.highQP = clampQP(qp + 3)
	c.lastQP = clampQP(qp)
}

func (c *Controller) regressionQP(targetBits float64) (int, bool) {
	if len(c.samples) < 2 {
		return 0, false
	}
	x := make([]float64, len(c.samples))
	y := make([]float64, len(c.samples))
	for i, s := range c.samples {
		x[i], y[i] = s.QP, s.Bits
	}
	alpha, beta := stat.LinearRegression(x, y, nil, false)
	if math.Abs(beta) < 1e-6 {
		return 0, false
	}
	qp := int(math.Round((math.Log(math.Max(1, targetBits)) - alpha) / beta))
	if qp < 1 || qp > 51 {
		return 0, false
	}
	return qp, true
}

func (c *Controller) clampAndProbe(qp int) int {
	qp = clampQP(qp)
	if len(c.samples) < 3 {
		mid := (c.lowQP + c.highQP) / 2
		if qp < c.lowQP {
			qp = c.lowQP
		} else if qp > c.highQP {
			qp = c.highQP
		}
		if qp == c.lastQP {
			qp = mid
		}
	}
	return clampQP(qp)
}

func (c *Controller) Snapshot() BufferState {
	return BufferState{
		LevelBits:    c.level,
		CapacityBits: c.capacity,
		CumTarget:    c.cumTarget,
		CumActual:    c.cumActual,
		Frames:       c.frames,
	}
}

func (c *Controller) Restore(s BufferState) {
	c.level = math.Max(0, math.Min(c.capacity, s.LevelBits))
	if s.CapacityBits > 0 {
		c.capacity = s.CapacityBits
	}
	c.cumTarget = s.CumTarget
	c.cumActual = s.CumActual
	c.frames = s.Frames
}

func (c *Controller) DeviationPct() float64 {
	if c.cumTarget <= 0 {
		return 0
	}
	return (c.cumActual - c.cumTarget) / c.cumTarget * 100
}

func (c *Controller) SceneResets() int { return c.sceneResets }

func (c *Controller) LastQP() int { return c.lastQP }

func clampQP(qp int) int {
	if qp < 1 {
		return 1
	}
	if qp > 51 {
		return 51
	}
	return qp
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
