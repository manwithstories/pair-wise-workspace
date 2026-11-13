package report

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

var failureOrder = []string{"timeout", "refused", "http_4xx", "http_5xx", "rate_limited", "parse", "network"}

type RunStats struct {
	RunID      string         `json:"run_id,omitempty"`
	Mode       string         `json:"mode"`
	Fetched    int            `json:"fetched"`
	RawURL     int            `json:"raw_url"`
	UniqURL    int            `json:"uniq_url"`
	Dedup      int            `json:"dedup"`
	New        int            `json:"new"`
	Revised    int            `json:"revised"`
	Failed     int            `json:"failed"`
	Failures   map[string]int `json:"failures"`
	Watermark  string         `json:"watermark"`
	Rows       int            `json:"rows"`
	StartedAt  time.Time      `json:"started_at"`
	FinishedAt time.Time      `json:"finished_at"`
}

type RerunStats struct {
	New        int  `json:"new"`
	Revised    int  `json:"revised"`
	Rows       int  `json:"rows"`
	Idempotent bool `json:"idempotent"`
	BeforeRows int  `json:"before_rows,omitempty"`
}

type Report struct {
	Full        *RunStats   `json:"full"`
	Incremental *RunStats   `json:"incr"`
	Rerun       *RerunStats `json:"rerun"`
}

func DedupRate(stats RunStats) float64 {
	if stats.RawURL == 0 {
		return 0
	}
	return float64(stats.Dedup) / float64(stats.RawURL) * 100
}

func (s RunStats) FullLine() string {
	var b strings.Builder
	fmt.Fprintf(&b, "mode=%s fetched=%d raw_url=%d uniq_url=%d dedup=%d(%.1f%%) new=%d revised=%d failed=%d",
		s.Mode, s.Fetched, s.RawURL, s.UniqURL, s.Dedup, DedupRate(s), s.New, s.Revised, s.Failed)
	if s.Failed > 0 {
		b.WriteString("(")
		parts := make([]string, 0, len(s.Failures))
		for _, class := range failureOrder {
			if count := s.Failures[class]; count > 0 {
				parts = append(parts, fmt.Sprintf("%s=%d", class, count))
			}
		}
		var unknown []string
		for class, count := range s.Failures {
			if count <= 0 || containsClass(class) {
				continue
			}
			unknown = append(unknown, fmt.Sprintf("%s=%d", class, count))
		}
		sort.Strings(unknown)
		parts = append(parts, unknown...)
		b.WriteString(strings.Join(parts, ","))
		b.WriteString(")")
	}
	b.WriteString(" watermark=")
	b.WriteString(s.Watermark)
	return b.String()
}

func (s RunStats) IncrementalLine() string {
	return fmt.Sprintf("mode=%s fetched=%d new=%d revised=%d dedup=%d",
		s.Mode, s.Fetched, s.New, s.Revised, s.Dedup)
}

func (r RerunStats) Line() string {
	return fmt.Sprintf("rerun=new:%d revised:%d rows=%d idempotent=%t", r.New, r.Revised, r.Rows, r.Idempotent)
}

func (r Report) Lines() []string {
	var lines []string
	if r.Full != nil {
		lines = append(lines, r.Full.FullLine())
	}
	if r.Incremental != nil {
		lines = append(lines, r.Incremental.IncrementalLine())
	}
	if r.Rerun != nil {
		lines = append(lines, r.Rerun.Line())
	}
	return lines
}

func (r Report) WriteJSON(path string) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	out, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	out = append(out, '\n')
	return os.WriteFile(path, out, 0o644)
}

func containsClass(class string) bool {
	for _, known := range failureOrder {
		if known == class {
			return true
		}
	}
	return false
}
