package metrics

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"
)

// Stats is the stable terminal/QC metric set.
type Stats struct {
	Profile           string  `json:"profile"`
	Codec             string  `json:"codec"`
	Seed              int64   `json:"seed"`
	Packets           int     `json:"packets"`
	WirePackets       int     `json:"wire_packets,omitempty"`
	Duplicates        int     `json:"duplicates"`
	Conflicts         int     `json:"conflicts,omitempty"`
	Malformed         int     `json:"malformed,omitempty"`
	Recovered         int     `json:"recovered"`
	RecoverableLoss   int     `json:"recoverable_loss"`
	UnrecoverableGaps int     `json:"unrecoverable_gaps,omitempty"`
	RecoveryRate      float64 `json:"recovery_rate"`
	AVSyncP95MS       float64 `json:"av_sync_p95_ms"`
	E2EP95MS          float64 `json:"e2e_p95_ms"`
	FirstBlocked      string  `json:"first_blocked"`
	OutputSHA256      string  `json:"-"`
}

// Artifact is the deterministic, content-hashed pipeline product.
type Artifact struct {
	SchemaVersion string    `json:"schema_version"`
	Profile       string    `json:"profile"`
	Codec         string    `json:"codec"`
	Seed          int64     `json:"seed"`
	Stats         Stats     `json:"stats"`
	E2ESamplesMS  []float64 `json:"e2e_samples_ms"`
	AVAbsMS       []float64 `json:"av_abs_ms"`
}

// Percentile returns a nearest-rank percentile in [0,1].
func Percentile(values []float64, p float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	if p <= 0 {
		return sorted[0]
	}
	if p >= 1 {
		return sorted[len(sorted)-1]
	}
	rank := int(math.Ceil(p*float64(len(sorted)))) - 1
	if rank < 0 {
		rank = 0
	}
	if rank >= len(sorted) {
		rank = len(sorted) - 1
	}
	return sorted[rank]
}

// CanonicalJSON serializes a value with deterministic field ordering. Struct
// field order is fixed; map keys are sorted by encoding/json.
func CanonicalJSON(v any) ([]byte, error) {
	return json.MarshalIndent(v, "", "  ")
}

// HashArtifact canonically serializes and hashes a pipeline artifact.
func HashArtifact(a Artifact) (string, []byte, error) {
	raw, err := CanonicalJSON(a)
	if err != nil {
		return "", nil, err
	}
	sum := sha256Bytes(raw)
	return fmt.Sprintf("%x", sum), raw, nil
}

// PrintKeyValues writes the acceptance metric keys in stable order.
func (s Stats) PrintKeyValues(w io.Writer) {
	firstBlocked := s.FirstBlocked
	if firstBlocked == "" {
		firstBlocked = "none"
	}
	_, _ = fmt.Fprintf(w, "packets=%d\n", s.Packets)
	_, _ = fmt.Fprintf(w, "duplicates=%d\n", s.Duplicates)
	_, _ = fmt.Fprintf(w, "recovered=%d\n", s.Recovered)
	_, _ = fmt.Fprintf(w, "recoverable_loss=%d\n", s.RecoverableLoss)
	_, _ = fmt.Fprintf(w, "recovery_rate=%.3f\n", s.RecoveryRate)
	_, _ = fmt.Fprintf(w, "av_sync_p95_ms=%.2f\n", s.AVSyncP95MS)
	_, _ = fmt.Fprintf(w, "e2e_p95_ms=%.2f\n", s.E2EP95MS)
	_, _ = fmt.Fprintf(w, "first_blocked=%s\n", firstBlocked)
	_, _ = fmt.Fprintf(w, "output_sha256=%s\n", s.OutputSHA256)
}

func sha256Bytes(b []byte) [32]byte { return sha256.Sum256(b) }

// Thresholds reports whether the accepted deterministic trajectory passes.
func (s Stats) Thresholds() map[string]bool {
	return map[string]bool{
		"recovery_rate>=0.80": s.RecoveryRate >= 0.80,
		"av_sync_p95_ms<=20":  s.AVSyncP95MS <= 20,
		"e2e_p95_ms<=180":     s.E2EP95MS <= 180,
	}
}
