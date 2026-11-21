package main

import (
	"fmt"

	"github.com/archive/rtprestore/internal/checkpoint"
)

// loadRates reads the persisted run rates, defaulting to zero for older dirs.
func loadRates(dir string) (roundTrip, keyframe float64) {
	m := checkpoint.LoadMetrics(dir)
	return m.RoundTripRate, m.KeyframeHitRate
}

// compareRuns asserts two run directories recorded identical per-segment
// SHA-256 values in the same order. Any divergence is data corruption.
func compareRuns(a, b string) error {
	ha, err := checkpoint.ShardHashes(a)
	if err != nil {
		return err
	}
	hb, err := checkpoint.ShardHashes(b)
	if err != nil {
		return err
	}
	if len(ha) != len(hb) {
		return fmt.Errorf("segment count differs: %s=%d %s=%d", a, len(ha), b, len(hb))
	}
	for i := range ha {
		if ha[i] != hb[i] {
			return fmt.Errorf("segment %d sha256 differs between %s and %s", i, a, b)
		}
	}
	return nil
}
