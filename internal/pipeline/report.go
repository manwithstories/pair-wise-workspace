package pipeline

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"relaycheck/internal/clock"
	"relaycheck/internal/mapping"
	"relaycheck/internal/provenance"
	"relaycheck/internal/recovery"
)

// Report is the run summary; the six acceptance metrics are printed as
// key=value lines by Print.
type Report struct {
	Segments         int      `json:"segments"`
	Accepted         int      `json:"accepted"`
	Duplicates       int      `json:"duplicates"`
	Quarantined      int      `json:"quarantined"`
	Lost             int      `json:"lost"`
	Gaps             int      `json:"gaps"`
	Rollbacks        int      `json:"rollbacks"`
	Chunks           int      `json:"chunks"`
	ResumedChunks    int      `json:"resumed_chunks"`
	SyncMaxUs        int64    `json:"sync_max_us"`
	LossRecovery     float64  `json:"loss_recovery"`
	LatencyP95Ms     float64  `json:"latency_p95_ms"`
	PayloadHashMatch float64  `json:"payload_hash_match"`
	ResultsDigest    string   `json:"results_digest"`
	CorruptIndexTail bool     `json:"corrupt_index_tail,omitempty"`
	Notes            []string `json:"notes,omitempty"`
}

// BuildReport derives every metric from the provenance trace and the
// index, so a rerun over a completed index reproduces the same report.
func BuildReport(cfg Config, idx *recovery.Index, mapper *mapping.Mapper,
	traces []provenance.SegmentTrace, chunkCount int, notes []clock.Fault) *Report {
	rep := &Report{Chunks: chunkCount, ResumedChunks: idx.ResumedChunks(), CorruptIndexTail: idx.CorruptTail}
	for _, f := range notes {
		rep.Notes = append(rep.Notes, f.String())
	}
	timescales := map[uint32]uint32{}
	for _, t := range mapper.Tracks() {
		timescales[t.OutID] = t.Info.Timescale
	}
	var latencies []float64
	var compared, matched int
	var losses, unrecoverable int
	for _, tr := range traces {
		switch tr.Decision {
		case provenance.Accepted:
			rep.Accepted++
		case provenance.Duplicate:
			rep.Duplicates++
		case provenance.Quarantined:
			rep.Quarantined++
			losses++
		case provenance.Lost:
			rep.Lost++
			losses++
		}
		if tr.LatencyMs > 0 {
			latencies = append(latencies, tr.LatencyMs)
		}
		for _, rep1 := range tr.Repairs {
			if strings.HasPrefix(rep1, "mfhd gap") {
				rep.Gaps++
			}
			if strings.Contains(rep1, "rollback") {
				rep.Rollbacks++
			}
			if strings.Contains(rep1, "unrecoverable") {
				unrecoverable++
			}
		}
		if tr.Decision == provenance.Accepted {
			rep.SyncMaxUs = max(rep.SyncMaxUs, syncOfTrace(tr, timescales))
			for _, tt := range tr.Tracks {
				compared++
				if tt.PayloadSHA == tt.OutputSHA {
					matched++
				}
			}
		}
	}
	rep.Segments = rep.Accepted + rep.Quarantined + rep.Lost
	if losses > 0 {
		rep.LossRecovery = float64(losses-unrecoverable) / float64(losses)
	} else {
		rep.LossRecovery = 1
	}
	if compared > 0 {
		rep.PayloadHashMatch = float64(matched) / float64(compared)
	} else {
		rep.PayloadHashMatch = 1
	}
	rep.LatencyP95Ms = percentile95(latencies)
	rep.ResultsDigest = resultsDigest(idx, chunkCount)
	return rep
}

// syncOfTrace returns the maximum cross-track presentation skew of one
// segment in microseconds.
func syncOfTrace(tr provenance.SegmentTrace, timescales map[uint32]uint32) int64 {
	var us []int64
	for _, tt := range tr.Tracks {
		us = append(us, clock.ToMicros(tt.FirstDTS, timescales[tt.TrackID]))
	}
	var maxDiff int64
	for i := range us {
		for j := i + 1; j < len(us); j++ {
			d := us[i] - us[j]
			if d < 0 {
				d = -d
			}
			if d > maxDiff {
				maxDiff = d
			}
		}
	}
	return maxDiff
}

func percentile95(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	sort.Float64s(v)
	rank := int(math.Ceil(0.95 * float64(len(v))))
	return v[rank-1]
}

// resultsDigest folds every committed chunk digest into one digest.
func resultsDigest(idx *recovery.Index, chunkCount int) string {
	h := sha256.New()
	for c := 1; c <= chunkCount; c++ {
		if rec, ok := idx.Chunks[c]; ok {
			fmt.Fprintf(h, "%d:%s;", c, rec.OutDigest)
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

func writeReport(dir string, rep *Report) error {
	data, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(dir, "report.json"), data)
}

func readFileIfExists(path string) ([]byte, error) {
	return os.ReadFile(path)
}

// LoadReport reads the report written by a finished run.
func LoadReport(dir string) (*Report, error) {
	data, err := readFileIfExists(filepath.Join(dir, "report.json"))
	if err != nil {
		return nil, err
	}
	var rep Report
	if err := json.Unmarshal(data, &rep); err != nil {
		return nil, err
	}
	return &rep, nil
}

// Print writes the acceptance metrics as key=value lines.
func (r *Report) Print(w io.Writer) {
	fmt.Fprintf(w, "sync_max_us=%d\n", r.SyncMaxUs)
	fmt.Fprintf(w, "loss_recovery=%.4f\n", r.LossRecovery)
	fmt.Fprintf(w, "latency_p95_ms=%.3f\n", r.LatencyP95Ms)
	fmt.Fprintf(w, "payload_hash_match=%.4f\n", r.PayloadHashMatch)
	fmt.Fprintf(w, "quarantined=%d\n", r.Quarantined)
	fmt.Fprintf(w, "resumed_chunks=%d\n", r.ResumedChunks)
	fmt.Fprintf(w, "segments=%d\n", r.Segments)
	fmt.Fprintf(w, "accepted=%d\n", r.Accepted)
	fmt.Fprintf(w, "duplicates=%d\n", r.Duplicates)
	fmt.Fprintf(w, "lost=%d\n", r.Lost)
	fmt.Fprintf(w, "gaps=%d\n", r.Gaps)
	fmt.Fprintf(w, "rollbacks=%d\n", r.Rollbacks)
	fmt.Fprintf(w, "results_digest=%s\n", r.ResultsDigest)
}
