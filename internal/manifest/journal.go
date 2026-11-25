package manifest

import (
	"encoding/json"
	"os"

	"archivectl/internal/timeline"
)

// Journal is the deterministic checkpoint used by --resume.
type Journal struct {
	Version             int        `json:"version"`
	InputPath           string     `json:"input_path"`
	InputSHA256         string     `json:"input_sha256"`
	OutputPath          string     `json:"output_path"`
	ManifestPath        string     `json:"manifest_path"`
	Window              int        `json:"window"`
	TargetKBPS          int        `json:"target_kbps"`
	Complete            bool       `json:"complete"`
	Partial             bool       `json:"partial"`
	ExitCode            int        `json:"exit_code"`
	OutputSHA256        string     `json:"output_sha256"`
	ManifestSHA256      string     `json:"manifest_sha256"`
	DupDrop             int        `json:"dup_drop"`
	Reordered           int        `json:"reordered"`
	Gaps                int        `json:"gaps"`
	GapRanges           int        `json:"gap_ranges"`
	Fabricated          int        `json:"fabricated"`
	PayloadHashMismatch int        `json:"payload_hash_mismatch"`
	ActualKBPS          float64    `json:"actual_kbps"`
	DeviationPct        float64    `json:"deviation_pct"`
	GapLedger           []GapRange `json:"gap_ledger,omitempty"`
}

// GapRange is an exact absent extended-sequence interval in the journal.
type GapRange struct {
	Start  int64  `json:"start"`
	End    int64  `json:"end"`
	Count  int    `json:"count"`
	Reason string `json:"reason"`
}

// SaveJournal atomically writes formatted JSON journal.
func SaveJournal(path string, j Journal) error {
	if path == "" {
		return nil
	}
	b, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return timeline.Wrap(timeline.CodeInvalidArgument, "write journal", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return timeline.Wrap(timeline.CodeInvalidArgument, "rename journal", err)
	}
	return nil
}

// LoadJournal reads a prior checkpoint.
func LoadJournal(path string) (Journal, error) {
	var j Journal
	b, err := os.ReadFile(path)
	if err != nil {
		return j, timeline.Wrap(timeline.CodeInvalidArgument, "read journal", err)
	}
	if err := json.Unmarshal(b, &j); err != nil {
		return j, timeline.Wrap(timeline.CodeIntegrity, "parse journal", err)
	}
	return j, nil
}

// ValidateResume ensures a checkpoint belongs to the same invocation inputs.
func ValidateResume(old, now Journal) error {
	if old.InputSHA256 != now.InputSHA256 || old.Window != now.Window || old.TargetKBPS != now.TargetKBPS {
		return timeline.New(timeline.CodeIntegrity, "resume journal", "journal parameters do not match input")
	}
	return nil
}
