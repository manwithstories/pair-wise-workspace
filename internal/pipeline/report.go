// Package pipeline wires the parsers, transforms and validators into one
// deterministic run and produces the JSON summary used by the CLI and tests.
package pipeline

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"mediasync/internal/media"
)

// Version is the report schema version.
const Version = 1

// SegmentReport is one cut clip in the summary.
type SegmentReport struct {
	Camera  string   `json:"camera"`
	Index   int      `json:"index"`
	StartMS int64    `json:"start_ms"`
	EndMS   int64    `json:"end_ms"`
	Hash    string   `json:"sha256"`
	Hash64  string   `json:"sha256_64"`
	Degrade []string `json:"degrade,omitempty"`
	Silence []string `json:"silence,omitempty"`
}

// IssueReport wraps an issue with the run-level disposition.
type IssueReport struct {
	Stage     string `json:"stage"`
	Input     string `json:"input"`
	Code      string `json:"code"`
	File      string `json:"file,omitempty"`
	Record    int    `json:"record,omitempty"`
	Message   string `json:"message"`
	Offset    int64  `json:"offset"`
	HasOffset bool   `json:"has_offset"`
	PacketSeq uint32 `json:"packet_seq,omitempty"`
	HasSeq    bool   `json:"has_packet_seq"`
	Fatal     bool   `json:"fatal"`
	// Expected marks damage intentionally injected by the fixture generator
	// so the demo can exit 0 while still reporting full diagnostics.
	Expected bool `json:"expected"`
}

// StageReport records one stage's outcome.
type StageReport struct {
	Name      string `json:"name"`
	OK        bool   `json:"ok"`
	Detail    string `json:"detail"`
	Issues    int    `json:"issues"`
	ElapsedUS int64  `json:"elapsed_us"`
}

// Report is the full JSON summary.
type Report struct {
	Version              int             `json:"version"`
	Seed                 int64           `json:"seed"`
	RoundtripConsistency float64         `json:"roundtrip_consistency"`
	KeyframeHitRate      float64         `json:"keyframe_hit_rate"`
	SegmentCount         int             `json:"segment_count"`
	SegmentSHA256        []string        `json:"segment_sha256"`
	DuplicatePackets     int             `json:"duplicate_packets"`
	ReorderedPackets     int             `json:"reordered_packets"`
	BurstLostPackets     int             `json:"burst_lost_packets"`
	LostPackets          int             `json:"lost_packets"`
	ReceivedPackets      int             `json:"received_packets"`
	EmittedPackets       int             `json:"emitted_packets"`
	ErrorsWithOffset     int             `json:"errors_with_offset"`
	TotalIssues          int             `json:"total_issues"`
	RecoverableIssues    int             `json:"recoverable_issues"`
	ExpectedIssues       int             `json:"expected_issues"`
	DuplicateMatches     int             `json:"duplicate_matches"`
	SilenceRuns          int             `json:"silence_runs"`
	BandwidthKbps        []int           `json:"bandwidth_kbps,omitempty"`
	BandwidthWindows     int             `json:"bandwidth_windows"`
	ShapedDrops          int             `json:"bandwidth_shaped_drops"`
	LinkMinKbps          int             `json:"link_min_kbps"`
	LinkMaxKbps          int             `json:"link_max_kbps"`
	Stages               []StageReport   `json:"stages"`
	Segments             []SegmentReport `json:"segments"`
	Matches              []MatchReport   `json:"matches"`
	Issues               []IssueReport   `json:"issues"`
	Validated            bool            `json:"gates_passed"`

	fatal bool
}

// MatchReport describes one cross-camera duplicate group.
type MatchReport struct {
	Hash    string   `json:"sha256"`
	Hash64  string   `json:"sha256_64"`
	Cameras []string `json:"cameras"`
	Count   int      `json:"count"`
}

// ExitCode maps the report to the CLI contract: 0 success, 2 recoverable
// (unexpected) issues, 1 fatal or invalid input.
func (r *Report) ExitCode() int {
	if r.fatal {
		return 1
	}
	recoverable := false
	for _, is := range r.Issues {
		if is.Fatal {
			return 1
		}
		if !is.Expected {
			recoverable = true
		}
	}
	if recoverable {
		return 2
	}
	return 0
}

// Gate is a validation threshold applied to the report.
type Gate struct {
	Name  string
	Value float64
	Min   float64
}

// Gates returns the acceptance gates required by the demo.
func (r *Report) Gates() []Gate {
	return []Gate{
		{Name: "roundtrip_consistency", Value: r.RoundtripConsistency, Min: 0.99},
		{Name: "keyframe_hit_rate", Value: r.KeyframeHitRate, Min: 0.95},
	}
}

// GatesPass reports whether every gate is met.
func (r *Report) GatesPass() bool {
	for _, g := range r.Gates() {
		if g.Value < g.Min {
			return false
		}
	}
	return true
}

// WriteJSON writes the report atomically to path, refusing path traversal and
// oversized destinations.
func (r *Report) WriteJSON(path string) error {
	clean, err := SafeOutputPath(path)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(clean), ".mediasync-*.json")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, clean); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}

// SafeOutputPath resolves an output path and refuses anything that escapes
// the workspace. Writes are limited to the current project directory and the
// system temporary directory; ".." components, NUL bytes and directories are
// rejected outright.
func SafeOutputPath(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", media.New(media.StagePipeline, media.InputMP4, media.CodeInvalidArgument, "output path is empty")
	}
	if strings.ContainsRune(path, 0) {
		return "", media.New(media.StagePipeline, media.InputMP4, media.CodePathRejected, "output path contains NUL")
	}
	if len(path) > 4096 {
		return "", media.New(media.StagePipeline, media.InputMP4, media.CodePathRejected, "output path is too long")
	}
	for _, part := range strings.Split(filepath.ToSlash(path), "/") {
		if part == ".." {
			return "", media.New(media.StagePipeline, media.InputMP4, media.CodePathRejected,
				"output path %q contains a '..' component", path)
		}
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", media.New(media.StagePipeline, media.InputMP4, media.CodePathRejected, "cannot resolve output path: %v", err)
	}
	clean := filepath.Clean(abs)
	if info, err := os.Stat(clean); err == nil && info.IsDir() {
		return "", media.New(media.StagePipeline, media.InputMP4, media.CodeInvalidArgument, "output path %q is a directory", path)
	}
	if !withinAllowedRoot(clean) {
		return "", media.New(media.StagePipeline, media.InputMP4, media.CodePathRejected,
			"output path %q is outside the project and temporary directories", path)
	}
	return clean, nil
}

// withinAllowedRoot reports whether path sits under the project directory or
// the system temporary directory.
func withinAllowedRoot(path string) bool {
	for _, root := range allowedRoots() {
		if root == "" {
			continue
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			continue
		}
		if rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
			return true
		}
	}
	return false
}

func allowedRoots() []string {
	var roots []string
	if cwd, err := os.Getwd(); err == nil {
		roots = append(roots, cwd)
		if resolved, err := filepath.EvalSymlinks(cwd); err == nil {
			roots = append(roots, resolved)
		}
	}
	tmp := os.TempDir()
	roots = append(roots, tmp)
	if resolved, err := filepath.EvalSymlinks(tmp); err == nil {
		roots = append(roots, resolved)
	}
	// macOS exposes /tmp as a symlink into /private; both spellings are
	// legitimate temporary locations.
	for _, extra := range []string{"/tmp", "/private/tmp"} {
		if info, err := os.Stat(extra); err == nil && info.IsDir() {
			roots = append(roots, extra)
		}
	}
	return roots
}

// joinInts renders a slice of ints as a comma-separated list.
func joinInts(vals []int) string {
	parts := make([]string, len(vals))
	for i, v := range vals {
		parts[i] = fmt.Sprint(v)
	}
	return strings.Join(parts, ",")
}
