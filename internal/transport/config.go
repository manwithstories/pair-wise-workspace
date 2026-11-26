package transport

import "time"

const (
	DefaultMaxPacketSize      = 1 << 20
	DefaultMaxBufferedPackets = 4096
	DefaultMaxBufferedBytes   = 64 << 20
	DefaultMaxRetries         = 3
	DefaultBurstThreshold     = 2
)

type Config struct {
	StreamID           string
	MaxPacketSize      int
	MaxBufferedPackets int
	MaxBufferedBytes   int64
	MaxRetries         int
	BurstThreshold     int
	RTT                time.Duration
	BandwidthBps       int64
	MaxWindowPackets   int
}

func DefaultConfig(stream string) Config {
	return Config{
		StreamID:           stream,
		MaxPacketSize:      64 << 10,
		MaxBufferedPackets: 256,
		MaxBufferedBytes:   16 << 20,
		MaxRetries:         DefaultMaxRetries,
		BurstThreshold:     DefaultBurstThreshold,
		RTT:                80 * time.Millisecond,
		BandwidthBps:       2_000_000,
		MaxWindowPackets:   64,
	}
}

type Stats struct {
	Received        int   `json:"received"`
	Unique          int   `json:"unique"`
	Duplicate       int   `json:"duplicate"`
	Reordered       int   `json:"reordered"`
	BurstLoss       int   `json:"burst_loss"`
	Retransmit      int   `json:"retransmit"`
	Exhausted       int   `json:"exhausted"`
	BufferedPackets int   `json:"buffered_packets"`
	BufferedBytes   int64 `json:"buffered_bytes"`
	WindowPackets   int   `json:"window_packets"`
	BandwidthBps    int64 `json:"bandwidth_bps"`
}

func (c *Config) normalize() {
	if c.MaxPacketSize <= 0 {
		c.MaxPacketSize = DefaultMaxPacketSize
	}
	if c.MaxBufferedPackets <= 0 {
		c.MaxBufferedPackets = DefaultMaxBufferedPackets
	}
	if c.MaxBufferedBytes <= 0 {
		c.MaxBufferedBytes = DefaultMaxBufferedBytes
	}
	if c.MaxRetries <= 0 {
		c.MaxRetries = DefaultMaxRetries
	}
	if c.BurstThreshold <= 0 {
		c.BurstThreshold = DefaultBurstThreshold
	}
	if c.RTT <= 0 {
		c.RTT = 80 * time.Millisecond
	}
	if c.MaxWindowPackets <= 0 {
		c.MaxWindowPackets = 64
	}
}
