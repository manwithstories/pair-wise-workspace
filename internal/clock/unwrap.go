// Package clock reconstructs a trustworthy presentation clock from raw
// container timestamps. Containers such as MXF and MP4 store timestamps in a
// fixed number of bits; when the counter wraps, the raw integer jumps backwards
// by 2^bits. The unwrapper decides when a backwards jump is a genuine wrap
// (and expands it) versus real damage or a reordered packet (and rejects it).
package clock

import (
	"fmt"

	"avpipe/internal/media"
)

// WrapConfig controls wrap detection for one track.
type WrapConfig struct {
	// Bits is the timestamp width, e.g. 33 for MPEG-2 style PTS fields. A
	// value <= 0 disables wrap handling.
	Bits int
	// Threshold is the maximum backwards jump, expressed in ticks of the
	// track time base, that is still accepted as a wrap. Jumps larger than
	// this are treated as corruption. A disabled threshold (<=0) uses 2^bits/2.
	Threshold int64
	// MaxLayout is the most wraps a single track may accumulate. It bounds
	// the expanded timeline so a pathological stream cannot overflow.
	MaxWraps int
}

// DefaultWrapConfig is used when a caller does not specify a width.
func DefaultWrapConfig(bits int) WrapConfig {
	if bits <= 0 {
		bits = 33
	}
	return WrapConfig{Bits: bits, Threshold: 0, MaxWraps: 1 << 10}
}

// maxValue is 2^bits - 1, the largest raw value the field can hold.
func (c WrapConfig) maxValue() int64 {
	if c.Bits <= 0 || c.Bits > 62 {
		return 0
	}
	return int64(1)<<uint(c.Bits) - 1
}

func (c WrapConfig) wrapSpan() int64 {
	if c.Bits <= 0 || c.Bits > 62 {
		return 0
	}
	return int64(1) << uint(c.Bits)
}

// Threshold semantics: a backwards step is only treated as a wrap when the
// previous raw timestamp lies within Threshold ticks of the top of the field.
// That is the signature of a genuine modular counter wrap: the counter must
// have climbed close to 2^bits before it rolls over. Any other backwards step
// is a regression the pipeline must reject, never silently reorder.
func (c WrapConfig) threshold() int64 {
	if c.Threshold > 0 {
		if c.Threshold > c.wrapSpan() {
			return c.wrapSpan()
		}
		return c.Threshold
	}
	span := c.wrapSpan()
	if span == 0 {
		return 0
	}
	return span / 2
}

// ValidationError reports a backwards jump that is too large to be a wrap.
type ValidationError struct {
	Raw     int64
	Prev    int64
	Delta   int64
	Origin  media.Origin
	Message string
}

func (e *ValidationError) Error() string { return e.Message }

// Unwrapper turns raw container timestamps into a monotonically increasing
// timeline by expanding genuine 2^bits wraps.
type Unwrapper struct {
	cfg       WrapConfig
	step      int64 // how many ticks one wrap adds
	wraps     int
	prevRaw   int64
	prevValue int64
	started   bool
	origin    media.Origin
}

// NewUnwrapper builds an unwrapper. step is the timeline increment added to a
// timestamp for every wrap; pass the wrap span for a pure modular counter, or
// a larger value when a stream is known to have skipped content.
func NewUnwrapper(cfg WrapConfig, step int64, origin media.Origin) (*Unwrapper, error) {
	if cfg.Bits <= 0 {
		return nil, media.Errorf(media.KindUsage, origin, "clock.NewUnwrapper", "wrap bits must be positive, got %d", cfg.Bits)
	}
	if cfg.Bits > 62 {
		return nil, media.Errorf(media.KindUsage, origin, "clock.NewUnwrapper", "wrap bits %d exceeds 62", cfg.Bits)
	}
	if step <= 0 {
		step = cfg.wrapSpan()
	}
	if cfg.MaxWraps <= 0 {
		cfg.MaxWraps = 1 << 10
	}
	return &Unwrapper{cfg: cfg, step: step, origin: origin}, nil
}

// Wraps reports how many wrap expansions have been applied.
func (u *Unwrapper) Wraps() int { return u.wraps }

// Reset clears the unwrapper state, e.g. at a clean segment boundary.
func (u *Unwrapper) Reset() {
	u.wraps = 0
	u.prevRaw = 0
	u.prevValue = 0
	u.started = false
}

// Value resolves a raw timestamp to its expanded value.
//
// A forwards step is always accepted. A backwards step is accepted as a wrap
// only when its magnitude is within the configured threshold and the total
// wrap count stays under MaxWraps; otherwise it is a ValidationError.
func (u *Unwrapper) Value(raw int64) (int64, error) {
	maxV := u.cfg.maxValue()
	// A raw value outside the field width means the container lied about the
	// bit depth; reject rather than guess.
	if maxV > 0 && (raw < 0 || raw > maxV) {
		return 0, media.Errorf(media.KindCorrupt, u.origin, "clock.Value",
			"raw timestamp %d outside %d-bit range [0,%d]", raw, u.cfg.Bits, maxV)
	}
	if !u.started {
		u.started = true
		u.prevRaw = raw
		u.prevValue = raw
		return raw, nil
	}
	delta := raw - u.prevRaw
	if delta >= 0 {
		u.prevRaw = raw
		u.prevValue += delta
		return u.prevValue, nil
	}
	// Backwards jump. It is a genuine wrap only if the previous sample was
	// near the top of the field, i.e. the counter reached the wrap boundary
	// before rolling over. A backwards step from mid-field is a regression.
	topGap := maxV - u.prevRaw
	if topGap > u.cfg.threshold() {
		return 0, &ValidationError{
			Raw:   raw,
			Prev:  u.prevRaw,
			Delta: delta,
			Origin: u.origin,
			Message: fmt.Sprintf("clock: backwards jump %d ticks from mid-field raw %d (needs prev within %d of field top)",
				-delta, u.prevRaw, u.cfg.threshold()),
		}
	}
	// Additionally the post-wrap value must be small: a wrap lands near zero.
	if raw > u.cfg.threshold() {
		return 0, &ValidationError{
			Raw:   raw,
			Prev:  u.prevRaw,
			Delta: delta,
			Origin: u.origin,
			Message: fmt.Sprintf("clock: wrap destination raw %d too high (threshold %d)", raw, u.cfg.threshold()),
		}
	}
	if u.wraps+1 > u.cfg.MaxWraps {
		return 0, media.Errorf(media.KindPolicy, u.origin, "clock.Value",
			"wrap count exceeds limit %d", u.cfg.MaxWraps)
	}
	u.wraps++
	// New value = old value + remaining ticks to the field top + raw + 1.
	remaining := (maxV - u.prevRaw) + raw + 1
	u.prevValue += remaining + (u.step - u.cfg.wrapSpan())
	u.prevRaw = raw
	return u.prevValue, nil
}

// ValueUnchecked expands without validation; callers must only use it after
// confirming with Value that a sample is acceptable.
func (u *Unwrapper) ValueUnchecked(raw int64) int64 {
	v, err := u.Value(raw)
	if err != nil {
		return raw
	}
	return v
}

// Expanded reports the expanded value of a raw timestamp without advancing
// state. It is intended for look-ahead decisions.
func (u *Unwrapper) Expanded(raw int64) (int64, bool) {
	if !u.started {
		return raw, true
	}
	delta := raw - u.prevRaw
	if delta >= 0 {
		return u.prevValue + delta, true
	}
	if -delta > u.cfg.threshold() {
		return 0, false
	}
	remaining := (u.cfg.maxValue() - u.prevRaw) + raw + 1
	return u.prevValue + remaining, true
}
