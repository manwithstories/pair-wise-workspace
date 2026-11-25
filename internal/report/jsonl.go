// Package report writes one JSON line per demo case.
package report

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"sort"
)

// Line is one case result line on stdout.
type Line struct {
	Case            string  `json:"case"`
	SyncP95Ms       float64 `json:"sync_p95_ms"`
	LossRecoveryPct float64 `json:"loss_recovery_pct"`
	P95WindowMs     float64 `json:"p95_window_ms"`
	DamagedSpans    int     `json:"damaged_spans"`
	Digest          string  `json:"digest"`
	Status          string  `json:"status,omitempty"`
}

// WriteLine marshals l as one JSON line.
func WriteLine(w io.Writer, l Line) error {
	b, err := json.Marshal(l)
	if err != nil {
		return err
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

// DigestOf hashes the canonical JSON of v; identical content always
// yields the same digest.
func DigestOf(v any) string {
	b, _ := json.Marshal(v)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:16]
}

// P95 returns the 95th percentile of xs (nearest-rank); 0 for empty.
func P95(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	idx := (95*len(s) + 99) / 100 // ceil(0.95*n)
	return s[idx-1]
}
