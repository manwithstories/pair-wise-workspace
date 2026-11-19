// Package subtitle parses SRT and WebVTT subtitle files while preserving the
// raw byte offset of every record so failures can be reported precisely.
package subtitle

import (
	"mediasync/internal/media"
)

// Cue is one subtitle record mapped onto the shared timeline.
type Cue struct {
	Index     int    `json:"index"`
	StartNS   int64  `json:"start_ns"`
	EndNS     int64  `json:"end_ns"`
	Text      string `json:"text"`
	StartLine int    `json:"start_line"`
	Offset    int64  `json:"offset"`
	EndOffset int64  `json:"end_offset"`
	Overlap   bool   `json:"overlap,omitempty"`
}

// DurationNS is the cue length in nanoseconds, clamped at zero.
func (c Cue) DurationNS() int64 {
	if c.EndNS < c.StartNS {
		return 0
	}
	return c.EndNS - c.StartNS
}

// Result is the outcome of parsing one subtitle file.
type Result struct {
	Format    string         `json:"format"`
	Cues      []Cue          `json:"cues"`
	Issues    []*media.Issue `json:"issues"`
	Lines     int            `json:"lines"`
	Bytes     int            `json:"bytes"`
	Recovered int            `json:"recovered"`
}

// Fatal reports whether parsing stopped on an unrecoverable issue.
func (r *Result) Fatal() bool { return media.FirstFatal(r.Issues) != nil }

// RecoverableIssues returns the issues that did not stop parsing.
func (r *Result) RecoverableIssues() []*media.Issue {
	return media.Filter(r.Issues, func(is *media.Issue) bool { return is.Recoverable() })
}

// Text returns the concatenated plain text of all cues, in timeline order.
func (r *Result) Text() string {
	out := ""
	for _, c := range r.Cues {
		out += c.Content() + "\n"
	}
	return out
}
