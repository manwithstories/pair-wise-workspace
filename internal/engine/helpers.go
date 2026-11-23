package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"sort"

	"mediaroute/internal/fault"
	"mediaroute/internal/transport"
)

// digestFile computes the SHA-256 of a local input while enforcing the
// project file-size limit through the probe constants.
func digestFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fault.New(fault.CatIO, "open %s: %v", path, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, io.LimitReader(f, maxDigestBytes)); err != nil {
		return "", fault.New(fault.CatIO, "digest %s: %v", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// maxDigestBytes bounds how much a single digest pass reads.
const maxDigestBytes int64 = 512 << 20

func nominalBps(stages []float64) int64 {
	if len(stages) == 0 {
		return 6_000_000
	}
	// Use the peak stage as the nominal provisioned rate.
	peak := stages[0]
	for _, v := range stages {
		if v > peak {
			peak = v
		}
	}
	return int64(peak * 1_000_000)
}

// injectBandwidth feeds the jitter buffer a bandwidth estimate derived
// from the ordered stage list, advancing one stage per fixed packet count
// so the demo deterministically exercises slowdown and recovery.
func injectBandwidth(jb *transport.JitterBuffer, step int, nominal int64, stages []float64) {
	if len(stages) == 0 {
		return
	}
	const packetsPerStage = 200
	stage := stages[(step/packetsPerStage)%len(stages)]
	bps := int64(stage * 1_000_000)
	jb.ObserveBandwidth(bps, nominal)
}

func sortedKeys(m map[int64]bool) []int64 {
	out := make([]int64, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
