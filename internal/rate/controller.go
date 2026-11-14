package rate

import (
	"fmt"
	"math"
)

// Controller picks per-segment QP under a CBR budget with VBV constraints.
type Controller struct {
	TargetBitsPerSegment int64
	VBVSizeBits          int64
	FillBitsPerSegment   int64
	ModelWindowK         int
	MaxQPDelta           int

	model     *Model
	vbvLevel  int64
	carry     int64
	prevQP    int
	hasPrev   bool
	underflow int
}

// NewController builds a controller for a CBR channel of targetBps at fps.
func NewController(targetBps int64, fps, segmentFrames int) *Controller {
	bitsPerSeg := targetBps * int64(segmentFrames) / int64(fps)
	vbvSize := 4 * targetBps // four seconds of buffer
	return &Controller{
		TargetBitsPerSegment: bitsPerSeg,
		VBVSizeBits:          vbvSize,
		FillBitsPerSegment:   bitsPerSeg,
		ModelWindowK:         8,
		MaxQPDelta:           6,
		model:                NewModel(),
		vbvLevel:             vbvSize / 2,
		prevQP:               32,
	}
}

// clampQP restricts qp to [MinQP,MaxQP] and to +/-MaxQPDelta around prevQP.
func (c *Controller) clampQP(qp int) int {
	if c.hasPrev {
		if d := qp - c.prevQP; d > c.MaxQPDelta {
			qp = c.prevQP + c.MaxQPDelta
		} else if d < -c.MaxQPDelta {
			qp = c.prevQP - c.MaxQPDelta
		}
	}
	if qp < 1 {
		qp = 1
	}
	if qp > 51 {
		qp = 51
	}
	return qp
}

// DecideQP picks the QP for the next segment. Decisions depend only on
// state fed back via Update, so they are deterministic for a given history.
func (c *Controller) DecideQP() int {
	budget := c.TargetBitsPerSegment + c.carry
	// never plan to spend more than the VBV can supply this segment
	available := c.vbvLevel + c.FillBitsPerSegment
	// safety margin against model error
	safe := available * 3 / 4
	if budget > safe {
		budget = safe
	}
	if budget < c.TargetBitsPerSegment/8 {
		budget = c.TargetBitsPerSegment / 8
	}
	qp := c.prevQP
	if alpha, beta, ok := c.model.Fit(c.ModelWindowK); ok {
		pred := PredictQP(alpha, beta, budget)
		if !math.IsNaN(pred) && !math.IsInf(pred, 0) {
			qp = int(math.Round(pred))
		}
	} else if lastQP, lastBits, ok := c.model.Last(); ok {
		// one-point power-law fallback with assumed slope -1.9
		ratio := float64(lastBits) / float64(budget)
		if ratio < 0.01 {
			ratio = 0.01
		}
		pred := float64(lastQP) * math.Pow(ratio, 1.0/1.9)
		qp = int(math.Round(pred))
	}
	return c.clampQP(qp)
}

// Update records the outcome of a segment: actual bits at qp.
// Returns true if the VBV underflowed.
func (c *Controller) Update(qp int, bits int64) bool {
	c.model.Add(qp, bits)
	// budget carryover: underspend is lent forward, overspend is clawed back
	c.carry += c.TargetBitsPerSegment - bits
	maxCarry := 4 * c.TargetBitsPerSegment
	if c.carry > maxCarry {
		c.carry = maxCarry
	}
	if c.carry < -maxCarry {
		c.carry = -maxCarry
	}
	// VBV: fill at channel rate, drain actual bits
	c.vbvLevel += c.FillBitsPerSegment
	if c.vbvLevel > c.VBVSizeBits {
		c.vbvLevel = c.VBVSizeBits
	}
	c.vbvLevel -= bits
	under := false
	if c.vbvLevel < 0 {
		c.vbvLevel = 0
		c.underflow++
		under = true
	}
	c.prevQP = qp
	c.hasPrev = true
	return under
}

// VBVLevel returns the current buffer level in bits.
func (c *Controller) VBVLevel() int64 { return c.vbvLevel }

// VBVUnderflows returns how many segments underflowed the buffer.
func (c *Controller) VBVUnderflows() int { return c.underflow }

// Carry returns the current budget carryover in bits.
func (c *Controller) Carry() int64 { return c.carry }

// PrevQP returns the last used QP.
func (c *Controller) PrevQP() int { return c.prevQP }

// String summarizes controller state for logs.
func (c *Controller) String() string {
	return fmt.Sprintf("vbv=%d/%d carry=%d prevQP=%d obs=%d",
		c.vbvLevel, c.VBVSizeBits, c.carry, c.prevQP, c.model.Observations())
}
