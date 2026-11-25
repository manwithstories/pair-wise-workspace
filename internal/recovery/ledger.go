package recovery

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"

	"mediad/internal/clock"
	"mediad/internal/fault"
)

// VersionInfo pins all components that affect output provenance.
type VersionInfo struct {
	SpecVersion    string `json:"spec_version"`
	AdapterVersion int    `json:"adapter_version"`
	Pipeline       string `json:"pipeline"`
	GoMP4          string `json:"go_mp4"`
	PionRTP        string `json:"pion_rtp"`
}

// Record is one source fragment's complete provenance entry.
type Record struct {
	RecordID        string            `json:"record_id"`
	SegmentIndex    int               `json:"segment_index"`
	Period          int               `json:"period"`
	SourceURI       string            `json:"source_uri"`
	SourceSHA256    string            `json:"source_sha256"`
	InitURI         string            `json:"init_uri,omitempty"`
	InitSHA256      string            `json:"init_sha256,omitempty"`
	Params          Params            `json:"params"`
	Versions        VersionInfo       `json:"versions"`
	OutputSHA256    string            `json:"output_sha256"`
	OutputArtifacts map[string]string `json:"output_artifacts,omitempty"`
	TimeRanges      []clock.TimeRange `json:"time_ranges"`
	Keyframes       []int             `json:"keyframes,omitempty"`
	KeyframeCount   int               `json:"keyframe_count"`
	RTPExtSequence  uint64            `json:"rtp_ext_sequence,omitempty"`
	RTPExtTimestamp uint64            `json:"rtp_ext_timestamp,omitempty"`
}

// Ledger is an append-only JSONL provenance log with idempotent IDs.
type Ledger struct {
	Path    string
	Records []Record
	byID    map[string]int
}

func LedgerPath(root string) string {
	return filepath.Join(root, "output", "ledger.jsonl")
}

// LoadLedger reads a JSONL ledger. Missing files yield an empty ledger.
func LoadLedger(root string) (*Ledger, error) {
	path := LedgerPath(root)
	l := &Ledger{Path: path, byID: map[string]int{}}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return l, nil
		}
		return nil, fault.New(fault.IO, path, -1, "open ledger: %v", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	line := 0
	for sc.Scan() {
		line++
		var r Record
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			return nil, fault.New(fault.LedgerCorrupt, path, int64(line), "invalid ledger record: %v", err)
		}
		if old, ok := l.byID[r.RecordID]; ok {
			l.Records[old] = r
		} else {
			l.byID[r.RecordID] = len(l.Records)
			l.Records = append(l.Records, r)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fault.New(fault.LedgerCorrupt, path, int64(line), "read ledger: %v", err)
	}
	return l, nil
}

// Append appends a record unless its ID already exists.
func (l *Ledger) Append(r Record) (bool, error) {
	if _, exists := l.byID[r.RecordID]; exists {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(l.Path), 0o755); err != nil {
		return false, fault.New(fault.IO, l.Path, -1, "create ledger dir: %v", err)
	}
	f, err := os.OpenFile(l.Path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return false, fault.New(fault.IO, l.Path, -1, "open ledger for append: %v", err)
	}
	data, err := json.Marshal(r)
	if err != nil {
		_ = f.Close()
		return false, fault.New(fault.LedgerCorrupt, l.Path, -1, "encode record: %v", err)
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		_ = f.Close()
		return false, fault.New(fault.IO, l.Path, -1, "append record: %v", err)
	}
	if err := f.Close(); err != nil {
		return false, fault.New(fault.IO, l.Path, -1, "close ledger: %v", err)
	}
	l.byID[r.RecordID] = len(l.Records)
	l.Records = append(l.Records, r)
	return true, nil
}

// WriteRewritten atomically replaces the ledger with canonical records.
func (l *Ledger) WriteRewritten() error {
	if err := os.MkdirAll(filepath.Dir(l.Path), 0o755); err != nil {
		return fault.New(fault.IO, l.Path, -1, "create output dir: %v", err)
	}
	tmp := l.Path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return fault.New(fault.IO, tmp, -1, "create temp ledger: %v", err)
	}
	enc := json.NewEncoder(f)
	for _, r := range l.Records {
		if err := enc.Encode(r); err != nil {
			_ = f.Close()
			return fault.New(fault.LedgerCorrupt, tmp, -1, "encode ledger: %v", err)
		}
	}
	if err := f.Close(); err != nil {
		return fault.New(fault.IO, tmp, -1, "close temp ledger: %v", err)
	}
	if err := os.Rename(tmp, l.Path); err != nil {
		return fault.New(fault.IO, l.Path, -1, "replace ledger: %v", err)
	}
	return nil
}

// ByIndex returns records sorted by segment index after loading.
func (l *Ledger) ByIndex(i int) *Record {
	for j := range l.Records {
		if l.Records[j].SegmentIndex == i {
			return &l.Records[j]
		}
	}
	return nil
}
