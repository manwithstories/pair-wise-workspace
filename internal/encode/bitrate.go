package encode

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
)

// Config is the cache-relevant encoding/mux profile.
type Config struct {
	TargetKbps     int    `json:"target_kbps"`
	VideoCodec     string `json:"video_codec"`
	AudioCodec     string `json:"audio_codec"`
	Width          int    `json:"width"`
	Height         int    `json:"height"`
	FPSNumerator   int64  `json:"fps_num"`
	FPSDenominator int64  `json:"fps_den"`
	SampleRate     int    `json:"sample_rate"`
	Channels       int    `json:"channels"`
	GOPFrames      int    `json:"gop_frames"`
	RateControl    string `json:"rate_control"`
}

func DefaultConfig(targetKbps, seconds int) Config {
	_ = seconds
	if targetKbps <= 0 {
		targetKbps = 1200
	}
	return Config{
		TargetKbps: targetKbps, VideoCodec: "h264", AudioCodec: "aac",
		Width: 640, Height: 360, FPSNumerator: 30, FPSDenominator: 1,
		SampleRate: 48_000, Channels: 1, GOPFrames: 60, RateControl: "cbr-vbv",
	}
}

func (c Config) Normalize() Config {
	d := DefaultConfig(c.TargetKbps, 0)
	if c.TargetKbps <= 0 {
		c.TargetKbps = d.TargetKbps
	}
	if c.VideoCodec == "" {
		c.VideoCodec = d.VideoCodec
	}
	if c.AudioCodec == "" {
		c.AudioCodec = d.AudioCodec
	}
	if c.Width <= 0 {
		c.Width = d.Width
	}
	if c.Height <= 0 {
		c.Height = d.Height
	}
	if c.FPSNumerator == 0 || c.FPSDenominator == 0 {
		c.FPSNumerator, c.FPSDenominator = 30, 1
	}
	if c.SampleRate <= 0 {
		c.SampleRate = d.SampleRate
	}
	if c.Channels <= 0 {
		c.Channels = 1
	}
	if c.GOPFrames <= 0 {
		c.GOPFrames = int(c.FPSNumerator / c.FPSDenominator * 2)
	}
	if c.RateControl == "" {
		c.RateControl = d.RateControl
	}
	return c
}

func (c Config) SHA256() (string, error) {
	b, err := json.Marshal(c.Normalize())
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// Controller tracks bytes against the target bitrate.
type Controller struct {
	Config       Config
	DurationSec  float64
	TargetBytes  int64
	WrittenBytes int64
	PaddingBytes int64
}

func NewController(cfg Config, durationSec float64) *Controller {
	cfg = cfg.Normalize()
	if durationSec <= 0 {
		durationSec = 1
	}
	return &Controller{Config: cfg, DurationSec: durationSec,
		TargetBytes: int64(math.Round(float64(cfg.TargetKbps*1000) * durationSec / 8))}
}

func (r *Controller) Observe(n int64) { r.WrittenBytes += n }

// NeededPadding returns the ISO-BMFF free-box payload required to bring the
// delivered file to the exact target byte budget. It never pads above budget.
func (r *Controller) NeededPadding(currentBytes int64) int64 {
	need := r.TargetBytes - currentBytes
	// Eight bytes are consumed by the free-box header.
	if need <= 16 {
		return 0
	}
	return need - 8
}

func (r *Controller) ObservePadding(payload int64) {
	if payload > 0 {
		r.PaddingBytes += payload + 8
		r.WrittenBytes += payload + 8
	}
}

func (r *Controller) ActualKbps(totalBytes int64) int {
	if r.DurationSec <= 0 {
		return 0
	}
	return int(math.Round(float64(totalBytes*8) / 1000 / r.DurationSec))
}

func (r *Controller) DeviationPct(totalBytes int64) float64 {
	actual := float64(r.ActualKbps(totalBytes))
	if r.Config.TargetKbps == 0 {
		return 0
	}
	return math.Round(math.Abs(actual-float64(r.Config.TargetKbps))/float64(r.Config.TargetKbps)*10000) / 100
}

// WithinTolerance reports the required ±5% broadcast demo window.
func (r *Controller) WithinTolerance(totalBytes int64) bool {
	return r.DeviationPct(totalBytes) <= 5
}
