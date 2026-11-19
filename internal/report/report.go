// Package report emits the batch summary as JSON and maps it to the
// process exit code: 0 complete and verified, 2 located input errors but
// continued, 3 checkpoint or output verification failed.
package report

import (
	"encoding/json"
	"fmt"
	"io"

	"review/internal/issues"
)

// Summary is the machine-readable result of one run.
type Summary struct {
	Status               string          `json:"status"` // complete | interrupted | failed
	DamageOffsetBytes    int64           `json:"damage_offset_bytes"`
	RecoveredBlocks      int             `json:"recovered_blocks"`
	ResumeMs             int64           `json:"resume_ms"`
	FramesExported       int             `json:"frames_exported"`
	SHA256               string          `json:"sha256"`
	AlignmentErrorFrames int             `json:"alignment_error_frames"`
	BlocksTotal          int             `json:"blocks_total"`
	BlocksDone           int             `json:"blocks_done"`
	Issues               []issues.Issue  `json:"issues"`
	VerificationFailed   bool            `json:"-"`
}

// ExitCode maps the summary to the process exit code.
func (s Summary) ExitCode() int {
	if s.VerificationFailed {
		return 3
	}
	if len(s.Issues) > 0 {
		return 2
	}
	return 0
}

// PrintJSON writes the summary as indented JSON.
func (s Summary) PrintJSON(w io.Writer) {
	data, _ := json.MarshalIndent(s, "", "  ")
	fmt.Fprintln(w, string(data))
}

// PrintText writes the key metrics as key=value lines.
func (s Summary) PrintText(w io.Writer) {
	fmt.Fprintf(w, "status=%s\n", s.Status)
	fmt.Fprintf(w, "damage_offset_bytes=%d\n", s.DamageOffsetBytes)
	fmt.Fprintf(w, "recovered_blocks=%d\n", s.RecoveredBlocks)
	fmt.Fprintf(w, "resume_ms=%d\n", s.ResumeMs)
	fmt.Fprintf(w, "frames_exported=%d\n", s.FramesExported)
	fmt.Fprintf(w, "sha256=%s\n", s.SHA256)
	fmt.Fprintf(w, "alignment_error_frames=%d\n", s.AlignmentErrorFrames)
	fmt.Fprintf(w, "blocks_done=%d/%d\n", s.BlocksDone, s.BlocksTotal)
	fmt.Fprintf(w, "issues=%d\n", len(s.Issues))
}
