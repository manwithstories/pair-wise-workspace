package media

import (
	"sort"
	"sync"
)

// Code is a stable machine-readable quality and failure classification.
type Code string

const (
	CodeOK               Code = "ok"
	CodeSyncLost         Code = "sync_lost"
	CodeUnsupported      Code = "unsupported"
	CodeLengthInvalid    Code = "length_invalid"
	CodeCountInvalid     Code = "count_invalid"
	CodeResourceLimit    Code = "resource_limit"
	CodeCRCInvalid       Code = "crc_invalid"
	CodePSIVersion       Code = "psi_version_mismatch"
	CodeCCGap            Code = "continuity_count_gap"
	CodeRTPDuplicate     Code = "rtp_duplicate"
	CodeRTPOutsideWindow Code = "rtp_outside_window"
	CodeClockWrap        Code = "clock_wrap"
	CodeClockDrift       Code = "clock_drift"
	CodeLateReplay       Code = "late_replay"
	CodeUnfixableGap     Code = "unfixable_gap"
	CodeMappingInvalid   Code = "mapping_invalid"
	CodeHashMismatch     Code = "hash_mismatch"
	CodeManifestSkew     Code = "manifest_skew"
	CodeCheckpoint       Code = "checkpoint_error"
	CodeInternal         Code = "internal_error"
	CodeUnexpectedEOF    Code = "unexpected_eof"
)

// Error is the common trace record shared by all subsystems.
type Error struct {
	Code        Code   `json:"code"`
	CaseID      string `json:"case_id,omitempty"`
	TraceID     string `json:"trace_id,omitempty"`
	Component   string `json:"component"`
	Offset      int64  `json:"offset"`
	Recoverable bool   `json:"recoverable"`
	Retryable   bool   `json:"retryable"`
	Fatal       bool   `json:"fatal"`
	Message     string `json:"message"`
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	msg := string(e.Code)
	if e.Message != "" {
		msg += ": " + e.Message
	}
	if e.TraceID != "" {
		msg += " [trace=" + e.TraceID + "]"
	}
	return msg
}

// MarkRecoverable returns e with the recovery marker set.
func (e *Error) MarkRecoverable() *Error {
	e.Recoverable = true
	e.Fatal = false
	return e
}

// MarkFatal returns e with the terminal marker set.
func (e *Error) MarkFatal() *Error {
	e.Fatal = true
	e.Recoverable = false
	return e
}

// Limits bounds parser and pipeline memory.
type Limits struct {
	MaxPacketBytes   int `json:"max_packet_bytes"`
	MaxPackets       int `json:"max_packets"`
	MaxSectionBytes  int `json:"max_section_bytes"`
	MaxPESBytes      int `json:"max_pes_bytes"`
	MaxSamples       int `json:"max_samples"`
	MaxReorderWindow int `json:"max_reorder_window"`
	MaxRewindEvents  int `json:"max_rewind_events"`
}

// DefaultLimits returns conservative local batch limits.
func DefaultLimits() Limits {
	return Limits{
		MaxPacketBytes:   64 * 1024,
		MaxPackets:       200_000,
		MaxSectionBytes:  4 * 1024,
		MaxPESBytes:      1024 * 1024,
		MaxSamples:       200_000,
		MaxReorderWindow: 64,
		MaxRewindEvents:  256,
	}
}

// Collector is a bounded, concurrency-safe error and trace sink.
type Collector struct {
	mu      sync.Mutex
	items   []*Error
	limit   int
	dropped int
}

// NewCollector creates a collector retaining at most limit records.
func NewCollector(limit int) *Collector {
	if limit <= 0 {
		limit = 4096
	}
	return &Collector{limit: limit}
}

// Add stores err after assigning case/trace context when supplied.
func (c *Collector) Add(err *Error) *Error {
	if err == nil || c == nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.items) >= c.limit {
		c.dropped++
		return err
	}
	cp := *err
	c.items = append(c.items, &cp)
	return &cp
}

// Items returns a sorted-by-offset copy of trace records.
func (c *Collector) Items() []*Error {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]*Error, len(c.items))
	copy(out, c.items)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Offset != out[j].Offset {
			return out[i].Offset < out[j].Offset
		}
		return out[i].Code < out[j].Code
	})
	return out
}

// Count reports occurrences of a code.
func (c *Collector) Count(code Code) int {
	n := 0
	for _, e := range c.Items() {
		if e.Code == code {
			n++
		}
	}
	return n
}

// HasFatal reports whether a terminal error was collected.
func (c *Collector) HasFatal() bool {
	for _, e := range c.Items() {
		if e.Fatal {
			return true
		}
	}
	return false
}
