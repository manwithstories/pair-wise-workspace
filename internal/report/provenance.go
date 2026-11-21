// Package report writes reviewable JSONL provenance records that bind every
// conclusion to its input digests, index version, checkpoint position, fault
// types and byte/time ranges.
package report

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"mediascan/internal/index"
	"mediascan/internal/model"
	"mediascan/internal/recover"
)

// Record is one JSONL line of provenance.
type Record struct {
	Kind          string         `json:"kind"` // "run" | "fault" | "summary"
	At            string         `json:"at"`
	ManifestSHA   string         `json:"input_sha256"`
	ResultSHA     string         `json:"result_sha256"`
	IndexVersion  int            `json:"index_version"`
	IndexSHA      string         `json:"index_sha256,omitempty"`
	Checkpoint    int            `json:"checkpoint_next_segment"`
	CheckpointSHA string         `json:"checkpoint_sha256,omitempty"`
	Fault         *model.Fault   `json:"fault,omitempty"`
	Counts        map[string]int `json:"fault_counts,omitempty"`
	Detail        string         `json:"detail,omitempty"`
}

// Provenance owns the JSONL output path and the current run's identifiers.
type Provenance struct {
	Path          string
	ManifestSHA   string
	ResultSHA     string
	IndexVersion  int
	IndexSHA      string
	Checkpoint    int
	CheckpointSHA string
	Now           func() time.Time
}

// New creates the JSONL path inside a work directory and truncates any prior
// record so a rerun cannot mix old and new conclusions.
func New(work string) (*Provenance, error) {
	dir := filepath.Join(work, "report")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "provenance.jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	return &Provenance{Path: path, Now: time.Now}, nil
}

// Bind sets the run identity from an index and checkpoint.
func (p *Provenance) Bind(idx *index.Index, cp *recover.Checkpoint, resultSHA string) {
	p.ManifestSHA = idx.InputSHA
	p.ResultSHA = resultSHA
	p.IndexVersion = idx.Version
	p.Checkpoint = 0
	if cp != nil {
		p.Checkpoint = cp.NextSeg
	}
	data, err := json.Marshal(idx)
	if err == nil {
		p.IndexSHA = index.HashBytes(data)
	}
	if cp != nil {
		cpData, err := json.Marshal(cp)
		if err == nil {
			p.CheckpointSHA = index.HashBytes(cpData)
		}
	}
}

// Write appends one record.
func (p *Provenance) Write(r Record) error {
	if p.Now != nil {
		r.At = p.Now().UTC().Format(time.RFC3339)
	}
	r.ManifestSHA = p.ManifestSHA
	r.ResultSHA = p.ResultSHA
	r.IndexVersion = p.IndexVersion
	r.IndexSHA = p.IndexSHA
	r.Checkpoint = p.Checkpoint
	r.CheckpointSHA = p.CheckpointSHA
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(p.Path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(data, '\n'))
	return err
}

// WriteFaults emits one record per fault plus a count summary.
func (p *Provenance) WriteFaults(faults []model.Fault) error {
	counts := map[string]int{}
	for i := range faults {
		f := faults[i]
		if err := f.Validate(); err != nil {
			return fmt.Errorf("report: refusing invalid fault: %w", err)
		}
		counts[string(f.Kind)]++
		if err := p.Write(Record{Kind: "fault", Fault: &f}); err != nil {
			return err
		}
	}
	return p.Write(Record{Kind: "summary", Counts: counts})
}

// VerifyResult checks stored provenance digests against a freshly computed
// result. It returns the first mismatch as an error.
func VerifyResult(path, manifestSHA, resultSHA string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	sawRun := false
	for sc.Scan() {
		var r Record
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			return fmt.Errorf("report: malformed JSONL line: %w", err)
		}
		if r.Kind != "summary" {
			continue
		}
		sawRun = true
		if r.ManifestSHA != manifestSHA {
			return fmt.Errorf("report: input digest mismatch: recorded %s, current %s", r.ManifestSHA, manifestSHA)
		}
		if resultSHA != "" && r.ResultSHA != resultSHA {
			return fmt.Errorf("report: result digest mismatch: recorded %s, current %s", r.ResultSHA, resultSHA)
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if !sawRun {
		return fmt.Errorf("report: no summary record in %s", path)
	}
	return nil
}
