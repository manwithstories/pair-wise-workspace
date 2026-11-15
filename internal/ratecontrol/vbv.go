// Package ratecontrol contains the VBV buffer model and two-pass bit allocator.
package ratecontrol

// VBV tracks a video buffering verifier. Fullness is measured in bits.
type VBV struct {
	CapacityBits       float64
	TargetBitsPerFrame float64
	FullnessBits       float64
}

// NewVBV creates a half-full buffer with a four-frame capacity.
func NewVBV(targetBitsPerFrame float64) *VBV {
	if targetBitsPerFrame < 1 {
		targetBitsPerFrame = 1
	}
	capacity := 4 * targetBitsPerFrame
	return &VBV{CapacityBits: capacity, TargetBitsPerFrame: targetBitsPerFrame, FullnessBits: capacity / 2}
}

// Observe records actual frame usage; a full buffer can spend more bits later.
func (v *VBV) Observe(actualBits float64) {
	if v.CapacityBits <= 0 {
		return
	}
	v.FullnessBits += v.TargetBitsPerFrame - actualBits
	if v.FullnessBits < 0 {
		v.FullnessBits = 0
	}
	if v.FullnessBits > v.CapacityBits {
		v.FullnessBits = v.CapacityBits
	}
}

// QMultiplier tightens quantization when the buffer is low and relaxes it when full.
func (v *VBV) QMultiplier() float64 {
	if v.CapacityBits <= 0 {
		return 1
	}
	ratio := v.FullnessBits / v.CapacityBits
	mult := 1.0
	switch {
	case ratio < 0.35:
		mult += (0.35 - ratio) * 1.4
	case ratio > 0.72:
		mult -= (ratio - 0.72) * 0.85
	}
	if mult < 0.72 {
		return 0.72
	}
	if mult > 1.45 {
		return 1.45
	}
	return mult
}

// Fullness returns the current buffer occupancy in [0,1].
func (v *VBV) Fullness() float64 {
	if v.CapacityBits <= 0 {
		return 0
	}
	return v.FullnessBits / v.CapacityBits
}
