package probe

// Config bounds every allocation and loop in the probe. Zero fields take the
// matching default from DefaultConfig.
type Config struct {
	// SegmentMS is the bucket width used for per-segment accounting.
	SegmentMS int64
	// CacheBytes caps reusable read buffers.
	CacheBytes int64
	// MaxMoovBytes caps the in-memory moov payload.
	MaxMoovBytes int64
	// MaxSamples caps decoded sample rows per track.
	MaxSamples uint64
	// MaxSegments caps per-track segment buckets.
	MaxSegments int
	// MaxKeyframes caps retained keyframe boundaries.
	MaxKeyframes int
	// MaxIssues caps retained faults per track.
	MaxIssues int
	// MaxDecodeBytes caps a single go-mp4 payload decode.
	MaxDecodeBytes int64
	// ReadBuffer is the reusable buffer used to validate sample payloads.
	ReadBuffer int64
}

// DefaultConfig returns limits suitable for multi-gigabyte inputs.
func DefaultConfig() Config {
	return Config{
		SegmentMS:      2000,
		CacheBytes:     4 << 20,
		MaxMoovBytes:   defaultMaxMoovBytes,
		MaxSamples:     20_000_000,
		MaxSegments:    100000,
		MaxKeyframes:   100000,
		MaxIssues:      32,
		MaxDecodeBytes: 8 << 20,
		ReadBuffer:     defaultReadBuffer,
	}
}

// Normalize replaces non-positive fields with defaults.
func (c Config) Normalize() Config {
	d := DefaultConfig()
	if c.SegmentMS <= 0 {
		c.SegmentMS = d.SegmentMS
	}
	if c.CacheBytes <= 0 {
		c.CacheBytes = d.CacheBytes
	}
	if c.MaxMoovBytes <= 0 {
		c.MaxMoovBytes = d.MaxMoovBytes
	}
	if c.MaxSamples <= 0 {
		c.MaxSamples = d.MaxSamples
	}
	if c.MaxSegments <= 0 {
		c.MaxSegments = d.MaxSegments
	}
	if c.MaxKeyframes <= 0 {
		c.MaxKeyframes = d.MaxKeyframes
	}
	if c.MaxIssues <= 0 {
		c.MaxIssues = d.MaxIssues
	}
	if c.MaxDecodeBytes <= 0 {
		c.MaxDecodeBytes = d.MaxDecodeBytes
	}
	if c.ReadBuffer <= 0 {
		c.ReadBuffer = d.ReadBuffer
	}
	if c.ReadBuffer > 8<<20 {
		c.ReadBuffer = 8 << 20
	}
	if c.MaxDecodeBytes > c.MaxMoovBytes {
		c.MaxDecodeBytes = c.MaxMoovBytes
	}
	return c
}
