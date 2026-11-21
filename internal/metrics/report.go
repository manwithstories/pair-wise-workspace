package metrics

import (
	"fmt"
	"sort"
	"strings"
)

// Report accumulates ordered key=value fields, stage failures and
// metric target checks, then renders them for the terminal.
type Report struct {
	keys     []string
	vals     map[string]string
	failures []string
	checks   []check
}

type check struct {
	name   string
	ok     bool
	detail string
}

// New creates an empty report.
func New() *Report {
	return &Report{vals: map[string]string{}}
}

// Set records a key=value field (last write wins, first order kept).
func (r *Report) Set(key, value string) {
	if _, ok := r.vals[key]; !ok {
		r.keys = append(r.keys, key)
	}
	r.vals[key] = value
}

// Setf records a formatted key=value field.
func (r *Report) Setf(key, format string, args ...interface{}) {
	r.Set(key, fmt.Sprintf(format, args...))
}

// AddFailure records a locatable processing failure (corrupt fixture,
// parse error, timeline issue). Failures never abort the pipeline.
func (r *Report) AddFailure(stage, detail string) {
	r.failures = append(r.failures, stage+": "+detail)
}

// Failures returns the recorded failures.
func (r *Report) Failures() []string {
	out := make([]string, len(r.failures))
	copy(out, r.failures)
	return out
}

// Check records a metric target verification.
func (r *Report) Check(name string, ok bool, detail string) {
	r.checks = append(r.checks, check{name: name, ok: ok, detail: detail})
}

// ChecksPass reports whether every metric check passed.
func (r *Report) ChecksPass() bool {
	for _, c := range r.checks {
		if !c.ok {
			return false
		}
	}
	return true
}

// Render produces the deterministic terminal output.
func (r *Report) Render() string {
	var b strings.Builder
	keys := make([]string, len(r.keys))
	copy(keys, r.keys)
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "%s=%s\n", k, r.vals[k])
	}
	for _, c := range r.checks {
		status := "pass"
		if !c.ok {
			status = "FAIL"
		}
		fmt.Fprintf(&b, "check_%s=%s (%s)\n", c.name, status, c.detail)
	}
	for i, f := range r.failures {
		fmt.Fprintf(&b, "failure_%d=%s\n", i+1, f)
	}
	return b.String()
}
