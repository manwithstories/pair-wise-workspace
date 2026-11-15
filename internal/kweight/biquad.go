package kweight

import "fmt"

// Biquad is a transposed direct-form II section. Each instance carries its
// own delay state, so channels and chunks remain independent and seamless.
type Biquad struct {
	c      Coefficients
	s1, s2 float64
}

func (b *Biquad) Reset() { b.s1, b.s2 = 0, 0 }

func (b *Biquad) Process(x float64) float64 {
	y := b.c.B0*x + b.s1
	b.s1 = b.c.B1*x - b.c.A1*y + b.s2
	b.s2 = b.c.B2*x - b.c.A2*y
	return y
}

// Chain is the shelf followed by the high-pass stage for one channel.
type Chain struct {
	shelf Biquad
	hp    Biquad
}

func newChain(d Design) Chain {
	return Chain{shelf: Biquad{c: d.Shelf}, hp: Biquad{c: d.HighPass}}
}

func (c *Chain) Reset() {
	c.shelf.Reset()
	c.hp.Reset()
}

func (c *Chain) Process(x float64) float64 {
	return c.hp.Process(c.shelf.Process(x))
}

// Bank filters interleaved samples, keeping one Chain per channel.
type Bank struct {
	design Design
	chains []Chain
}

func NewBank(d Design, channels int) (*Bank, error) {
	if channels <= 0 {
		return nil, fmt.Errorf("channel count must be positive")
	}
	b := &Bank{design: d, chains: make([]Chain, channels)}
	b.resetChains()
	return b, nil
}

// Reset prepares the bank for a new file, optionally with a new layout.
func (b *Bank) Reset(d Design, channels int) error {
	if channels <= 0 {
		return fmt.Errorf("channel count must be positive")
	}
	b.design = d
	if cap(b.chains) < channels {
		b.chains = make([]Chain, channels)
	} else {
		b.chains = b.chains[:channels]
	}
	b.resetChains()
	return nil
}

func (b *Bank) resetChains() {
	for i := range b.chains {
		b.chains[i] = newChain(b.design)
	}
}

// Process filters src into dst using the same interleaved layout. The two
// slices may refer to the same memory.
func (b *Bank) Process(dst, src []float64, frames int) error {
	channels := len(b.chains)
	if frames < 0 || len(src) < frames*channels || len(dst) < frames*channels {
		return fmt.Errorf("filter buffers too small")
	}
	for i := 0; i < frames; i++ {
		base := i * channels
		for ch := range b.chains {
			dst[base+ch] = b.chains[ch].Process(src[base+ch])
		}
	}
	return nil
}
