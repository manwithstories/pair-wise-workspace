package quality

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"

	"mediaflow/internal/clock"
)

// Fingerprint is a cheap rolling hash over a frame's payload plus timing. It
// is only used to nominate duplicate candidates; it never authorizes deletion.
type Fingerprint struct {
	Track   uint32
	Frame   int
	StartUS int64
	Hash    uint64
}

// Match is a verified duplicate relationship after the second-stage check.
type Match struct {
	Track       uint32
	FrameA      int
	FrameB      int
	OffsetUS    int64 // time offset between the two aligned copies
	ByteEqual   bool
	Fingerprint uint64
}

// FingerprintConfig controls candidate nomination and verification.
type FingerprintConfig struct {
	// WindowFrames bounds how far back a candidate may match.
	WindowFrames int
	// MaxOffsetUS limits alignment drift accepted during verification.
	MaxOffsetUS int64
}

// DefaultFingerprintConfig provides bounded demo windows.
func DefaultFingerprintConfig() FingerprintConfig {
	return FingerprintConfig{WindowFrames: 64, MaxOffsetUS: 500_000}
}

// FingerprintAll computes rolling fingerprints for normalized frames.
func FingerprintAll(frames []clock.Normalized) []Fingerprint {
	out := make([]Fingerprint, 0, len(frames))
	for i, f := range frames {
		out = append(out, Fingerprint{
			Track:   f.Track,
			Frame:   i,
			StartUS: f.StartUS,
			Hash:    rollingHash(f),
		})
	}
	return out
}

// rollingHash is FNV-1a over payload and timing — fast, but explicitly not
// collision-resistant enough to prove equality by itself.
func rollingHash(f clock.Normalized) uint64 {
	const offset64 = 14695981039346656037
	const prime64 = 1099511628211
	h := uint64(offset64)
	mix := func(b byte) {
		h ^= uint64(b)
		h *= prime64
	}
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], uint64(f.StartUS))
	for _, b := range buf {
		mix(b)
	}
	binary.LittleEndian.PutUint64(buf[:], uint64(f.Duration))
	for _, b := range buf {
		mix(b)
	}
	for _, b := range f.Payload {
		mix(b)
	}
	return h
}

// FindDuplicates performs two-stage detection:
//  1. fingerprint equality only nominates candidates within the window;
//  2. a full byte comparison plus time-offset alignment confirms the match.
//
// A hash collision with differing payload is rejected, so no content is ever
// deleted on a single hash.
func FindDuplicates(frames []clock.Normalized, cfg FingerprintConfig) []Match {
	fps := FingerprintAll(frames)
	byHash := map[uint64][]int{}
	var matches []Match
	for i, fp := range fps {
		candidates := byHash[fp.Hash]
		for _, j := range candidates {
			if i-j > cfg.WindowFrames {
				continue
			}
			offset := fp.StartUS - fps[j].StartUS
			if offset < 0 {
				offset = -offset
			}
			if offset > cfg.MaxOffsetUS {
				continue
			}
			// Stage 2: full byte equality is the actual duplicate proof.
			byteEqual := bytes.Equal(frames[i].Payload, frames[j].Payload)
			if !byteEqual {
				continue // hash collision: explicitly not a duplicate
			}
			matches = append(matches, Match{
				Track:       fp.Track,
				FrameA:      j,
				FrameB:      i,
				OffsetUS:    frames[i].StartUS - frames[j].StartUS,
				ByteEqual:   true,
				Fingerprint: fp.Hash,
			})
		}
		byHash[fp.Hash] = append(byHash[fp.Hash], i)
	}
	return matches
}

// HashPayload returns a strong SHA-256 digest for output integrity checks.
func HashPayload(b []byte) [32]byte {
	return sha256.Sum256(b)
}
