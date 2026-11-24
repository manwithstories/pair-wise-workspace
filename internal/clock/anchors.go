// Package clock fuses tfdt and RTP anchors, unwraps 32-bit RTP timestamps,
// estimates clock drift and produces monotonic sample timelines.
package clock

import (
	"encoding/base64"
	"encoding/json"
	"math"
	"os"
	"path/filepath"

	"github.com/pion/rtp"

	"bcast/internal/media"
)

// AnchorEnvelope is the deterministic sidecar written by the fixture.  The
// packet bytes are real marshalled RTP packets and are parsed with pion/rtp.
type AnchorEnvelope struct {
	ClockRate uint32         `json:"clock_rate"`
	Records   []AnchorRecord `json:"records"`
}

// AnchorRecord associates one RTP packet with its original fragment number.
type AnchorRecord struct {
	TrackID   uint32 `json:"track_id"`
	Fragment  uint32 `json:"fragment"`
	PacketB64 string `json:"packet_b64"`
}

// Anchor is a parsed and later unwrapped RTP clock anchor.
type Anchor struct {
	TrackID   uint32
	Fragment  uint32
	Sequence  uint16
	RTP32     uint32
	Unwrapped int64
	ClockRate uint32
}

// LoadAnchors reads and parses rtp_anchors.json.  A missing sidecar is not an
// I/O failure: tfdt-only repair remains possible, but regression repair is less
// independently verifiable.
func LoadAnchors(dir string) (map[uint32]map[uint32]*Anchor, error) {
	path := filepath.Join(dir, "rtp_anchors.json")
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[uint32]map[uint32]*Anchor{}, nil
		}
		return nil, media.IO("clock", path, 0, 0, err)
	}
	var env AnchorEnvelope
	if err := json.Unmarshal(b, &env); err != nil {
		return nil, media.Corrupt("clock", "invalid_rtp_anchors", path, 0, 0, "%v", err)
	}
	out := map[uint32]map[uint32]*Anchor{}
	for i, rec := range env.Records {
		raw, err := base64.StdEncoding.DecodeString(rec.PacketB64)
		if err != nil {
			return nil, media.Corrupt("clock", "invalid_rtp_base64", path, rec.Fragment, int64(i), "%v", err)
		}
		var pkt rtp.Packet
		if err := pkt.Unmarshal(raw); err != nil {
			return nil, media.Corrupt("clock", "invalid_rtp_packet", path, rec.Fragment, int64(i), "%v", err)
		}
		if env.ClockRate == 0 {
			return nil, media.Corrupt("clock", "invalid_rtp_clock_rate", path, rec.Fragment, 0, "clock rate is zero")
		}
		byTrack := out[rec.TrackID]
		if byTrack == nil {
			byTrack = map[uint32]*Anchor{}
			out[rec.TrackID] = byTrack
		}
		byTrack[rec.Fragment] = &Anchor{
			TrackID:   rec.TrackID,
			Fragment:  rec.Fragment,
			Sequence:  pkt.SequenceNumber,
			RTP32:     pkt.Timestamp,
			ClockRate: env.ClockRate,
		}
	}
	return out, nil
}

// UnwrapRTP extends a 32-bit RTP timestamp near expectedTicks.  The candidate
// wrap count nearest to expected is selected, so a wrapped or mildly drifted
// clock does not create a 2^32 discontinuity.
func UnwrapRTP(prev int64, current32 uint32, expected int64) int64 {
	const modulus = int64(1) << 32
	cur := int64(current32)
	if prev == 0 && expected == 0 {
		return cur
	}
	base := cur
	// Choose k such that cur+k*modulus is closest to expected.
	k := int64(math.Round(float64(expected-cur) / float64(modulus)))
	candidate := cur + k*modulus
	_ = base
	// Preserve strict forward progress if rounding picked an equal/behind value
	// by less than one packet interval.
	if prev != 0 && candidate < prev {
		candidate += modulus
	}
	return candidate
}

// estimateDriftPPM fits y = scale*x through origin after normalizing the first
// anchor to zero.  x and y use the same nominal clock rate.
func estimateDriftPPM(x, y []int64) float64 {
	if len(x) < 2 {
		return 0
	}
	var num, den float64
	x0, y0 := x[0], y[0]
	for i := range x {
		dx := float64(x[i] - x0)
		dy := float64(y[i] - y0)
		num += dx * dy
		den += dx * dx
	}
	if den == 0 {
		return 0
	}
	scale := num / den
	return (scale - 1) * 1_000_000
}
