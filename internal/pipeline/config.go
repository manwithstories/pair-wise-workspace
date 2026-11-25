// Package pipeline wires the kernel stages together deterministically. It is
// the only orchestration layer; cmd/relaycheck only parses flags and prints.
package pipeline

import (
	"strconv"
	"strings"

	"relaycheck/internal/media"
)

// SegmentCount is the fixed number of committed fragments and keyframes.
const SegmentCount = 120

// BlocksPerSegment RTP packets per 20 ms segment (8 * 120 frames = 960).
const BlocksPerSegment = 8

// FramesPerBlock is the shared-clock frame span of one RTP packet.
const FramesPerBlock = 120

// Config are the demo parameters. Defaults equal the acceptance command so a
// minimal crash/recover invocation reproduces the fully-specified run.
type Config struct {
	Seed              int64
	TargetRate        int
	Layout            string
	IntegratedLUFS    float64
	DuplicateEvery    int
	ReorderWindow     int
	BurstLoss         int
	BandwidthKbps     []int
	Out               string
	Case              string
	CrashAfterSegment int
	Resume            bool
}

// DefaultConfig returns the acceptance-calibrated configuration.
func DefaultConfig() Config {
	return Config{
		Seed:              25,
		TargetRate:        48000,
		Layout:            "stereo",
		IntegratedLUFS:    -18,
		DuplicateEvery:    17,
		ReorderWindow:     9,
		BurstLoss:         6,
		BandwidthKbps:     []int{12000, 4000, 9000},
		Out:               ".relay-out",
		CrashAfterSegment: -1,
	}
}

// ParseBandwidth parses "12000,4000,9000".
func ParseBandwidth(s string) ([]int, *media.Fault) {
	parts := strings.Split(s, ",")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		v, err := strconv.Atoi(p)
		if err != nil || v <= 0 {
			return nil, media.NewFault(media.CodeInvalidInput, "bad bandwidth step %q", p)
		}
		out = append(out, v)
	}
	if len(out) == 0 {
		return nil, media.NewFault(media.CodeInvalidInput, "no bandwidth steps in %q", s)
	}
	return out, nil
}
