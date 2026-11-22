// Package frame models the timestamped image-frame manifest and validates
// frame metadata, timestamp monotonicity, size limits and SHA-256 hashes.
package frame

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"mediascan/internal/clock"
	"mediascan/internal/fault"
)

// Limits guard every manifest against unbounded or integer-overflowing input.
const (
	// MaxFrameSize caps a single encoded frame at 16 MiB.
	MaxFrameSize = 16 << 20
	// MaxTotalBytes caps the summed frame payload at 1 GiB.
	MaxTotalBytes = 1 << 30
	// MaxFrames caps the frame count.
	MaxFrames = 1_000_000
	// HashLen is the hex-encoded SHA-256 length.
	HashLen = sha256.Size * 2
)

// Frame is one manifest entry. Payload is held outside the manifest and
// verified through Verify instead of being embedded in the JSON structure.
type Frame struct {
	Index      int    `json:"index"`
	ID         string `json:"id"`
	Keyframe   bool   `json:"keyframe"`
	Timestamp  int64  `json:"timestamp_ms"`
	Size       int    `json:"size"`
	SHA256     string `json:"sha256"`
}

// Manifest is the full ordered frame manifest.
type Manifest struct {
	FPS        int     `json:"fps"`
	ClockRate  int64   `json:"clock_rate"`
	GOP        int     `json:"gop"`
	TargetKbps int     `json:"target_kbps"`
	Frames     []Frame `json:"frames"`
}

// Result aggregates validation outcomes on the shared time base.
type Result struct {
	Faults       []fault.Fault
	TotalBytes   int64
	FirstTick    int64
	LastTick     int64
	AlignmentMS  int64 // max |actual tick - expected tick| expressed in ms
	KeyframeIdx  []int
}

// Validate checks metadata and timestamp rules without touching payloads.
// Every located defect is returned; fatal defects make the input invalid.
func (m *Manifest) Validate() (*Result, error) {
	if m.FPS <= 0 || m.ClockRate <= 0 {
		return nil, errors.New("frame: fps and clock_rate must be positive")
	}
	if m.GOP <= 0 {
		return nil, errors.New("frame: gop must be positive")
	}
	clk, err := clock.New(m.ClockRate, int64(m.FPS))
	if err != nil {
		return nil, fmt.Errorf("frame: %w", err)
	}
	if len(m.Frames) > MaxFrames {
		return nil, fmt.Errorf("frame: %d frames exceed limit %d", len(m.Frames), MaxFrames)
	}
	res := &Result{FirstTick: -1, LastTick: -1}
	var total int64
	seenTS := make(map[int64]int, len(m.Frames))
	step := clk.TicksPerFrame()
	var prevTick int64
	for i, fr := range m.Frames {
		if fr.Index != i {
			res.Faults = append(res.Faults, fault.Fault{
				Cat: fault.CatIllegalSize, Ref: fr.ID,
				Msg: fmt.Sprintf("frame index %d out of sequence, want %d", fr.Index, i),
			})
		}
		if fr.Size <= 0 || fr.Size > MaxFrameSize {
			res.Faults = append(res.Faults, fault.Fault{
				Cat: fault.CatSizeViolation, Ref: fr.ID,
				Msg: fmt.Sprintf("frame size %d outside (0,%d]", fr.Size, MaxFrameSize),
			})
		}
		total += int64(fr.Size)
		if len(fr.SHA256) != HashLen {
			res.Faults = append(res.Faults, fault.Fault{
				Cat: fault.CatBadChecksum, Ref: fr.ID,
				Msg: "sha256 must be 64 hex chars",
			})
		}
		tick, err := clk.FromMillis(fr.Timestamp)
		if err != nil {
			return nil, fmt.Errorf("frame %d: %w", i, err)
		}
		if i > 0 && tick <= prevTick {
			cat := fault.CatNonMonotonic
			if tick == prevTick {
				cat = fault.CatDupTimestamp
			}
			res.Faults = append(res.Faults, fault.Fault{
				Cat: cat, Span: fault.Interval{Start: tick, End: tick + step},
				Ref: fr.ID, Msg: fmt.Sprintf("timestamp %dms not strictly increasing", fr.Timestamp),
			})
		}
		if dup, ok := seenTS[fr.Timestamp]; ok {
			res.Faults = append(res.Faults, fault.Fault{
				Cat: fault.CatDupTimestamp, Ref: fmt.Sprintf("%s~%d", fr.ID, dup),
				Span: fault.Interval{Start: tick, End: tick + step},
				Msg: fmt.Sprintf("duplicate timestamp %dms", fr.Timestamp),
			})
		}
		seenTS[fr.Timestamp] = i
		// alignment against the constant-rate grid
		want, _ := clk.FrameTicks(int64(i))
		d := tick - want
		if d < 0 {
			d = -d
		}
		if ms := clk.Millis(d); ms > res.AlignmentMS {
			res.AlignmentMS = ms
		}
		if i == 0 {
			res.FirstTick = tick
		}
		res.LastTick = tick
		prevTick = tick
		if fr.Keyframe {
			res.KeyframeIdx = append(res.KeyframeIdx, i)
		}
	}
	if total > MaxTotalBytes {
		return nil, fmt.Errorf("frame: total %d bytes exceed limit %d", total, MaxTotalBytes)
	}
	if total < 0 {
		return nil, errors.New("frame: total byte count overflow")
	}
	res.TotalBytes = total
	return res, nil
}

// Verify checks one frame payload against its recorded SHA-256 hash.
func (f Frame) Verify(payload []byte) fault.Fault {
	sum := sha256.Sum256(payload)
	got := hex.EncodeToString(sum[:])
	if got != f.SHA256 {
		return fault.Fault{
			Cat:  fault.CatBadChecksum,
			Byte: -1,
			Ref:  f.ID,
			Msg:  fmt.Sprintf("hash mismatch: manifest=%s payload=%s", f.SHA256, got),
		}
	}
	return fault.Fault{}
}

// IsZero reports whether a returned Fault is empty (verification passed).
func IsZero(f fault.Fault) bool { return f.Cat == "" }
