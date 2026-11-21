package recover

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"mediascan/internal/index"
	"mediascan/internal/model"
)

// digestWriter accumulates bytes into a sha256 in the order written.
type digestWriter struct {
	buf []byte
}

func (d *digestWriter) Write(p []byte) (int, error) {
	d.buf = append(d.buf, p...)
	return len(p), nil
}

// Sum returns the hex digest of everything written so far.
func (d *digestWriter) Sum() string {
	sum := sha256.Sum256(d.buf)
	return hex.EncodeToString(sum[:])
}

// ResultDigest hashes a result payload in a stable, key-ordered encoding so a
// repeated run over unchanged inputs yields the same digest.
func ResultDigest(v any) (string, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// CheckpointForIndex builds a fresh checkpoint positioned after the last
// fully hashed entry, preserving any earlier interrupted position when the
// binding still matches.
func CheckpointForIndex(idx *index.Index, params Params, prev *Checkpoint) *Checkpoint {
	cp := &Checkpoint{
		ManifestSHA:  idx.InputSHA,
		SegmentSHAs:  segmentSetDigest(idx),
		Params:       params,
		IndexVersion: idx.Version,
		Status:       "running",
	}
	if prev != nil {
		if err := prev.Validate(BindingOf(idx, params)); err == nil {
			cp.NextSeg = prev.NextSeg
			cp.Completed = prev.Completed
			cp.Status = prev.Status
			cp.Results = prev.Results
		}
	}
	if cp.NextSeg == 0 {
		for i, e := range idx.Entries {
			if !e.Present {
				break
			}
			cp.NextSeg = i + 1
		}
		cp.Completed = cp.NextSeg
	}
	return cp
}

// SummaryFaults renders a compact fault list for logs and terminal output.
func SummaryFaults(fs []model.Fault) string {
	if len(fs) == 0 {
		return "none"
	}
	counts := map[model.FaultKind]int{}
	order := []model.FaultKind{}
	for _, f := range fs {
		if counts[f.Kind] == 0 {
			order = append(order, f.Kind)
		}
		counts[f.Kind]++
	}
	out := ""
	for i, k := range order {
		if i > 0 {
			out += ","
		}
		out += fmt.Sprintf("%s=%d", k, counts[k])
	}
	return out
}
