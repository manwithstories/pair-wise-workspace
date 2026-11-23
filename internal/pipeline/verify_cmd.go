package pipeline

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/example/mediad/internal/verify"
)

// VerifyReport is the verify-command output.
type VerifyReport struct {
	HashMatched int
	HashTotal   int
	Rejected    int
	AllMatch    bool
}

// VerifyManifest reads a manifest and independently recomputes every
// segment hash with the standard-library hasher.
func VerifyManifest(manifestPath string) (*VerifyReport, error) {
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, err
	}
	var doc ManifestFile
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	m := &verify.Manifest{Segments: doc.Segments}
	matched, err := verify.RecomputeHashes(m)
	if err != nil {
		return &VerifyReport{
			HashMatched: matched,
			HashTotal:   len(doc.Segments),
			Rejected:    len(doc.Rejected),
			AllMatch:    false,
		}, err
	}
	return &VerifyReport{
		HashMatched: matched,
		HashTotal:   len(doc.Segments),
		Rejected:    len(doc.Rejected),
		AllMatch:    matched == len(doc.Segments),
	}, nil
}

// ReportLine renders the verify report in the acceptance key=value form.
func (r VerifyReport) ReportLine() string {
	return fmt.Sprintf("segment_hash_match=%d/%d rejected_segments=%d all_match=%t",
		r.HashMatched, r.HashTotal, r.Rejected, r.AllMatch)
}
