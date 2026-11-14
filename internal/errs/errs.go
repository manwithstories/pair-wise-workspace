// Package errs defines the structured error type carried across pack,
// verify and the CLI. Every failure surfaces as a JSON object on stderr
// with a stable machine-readable code and a process exit code.
package errs

import "encoding/json"

// Exit codes.
const (
	ExitOK       = 0
	ExitParam    = 2 // parameter / usage errors, output exists, overflow
	ExitAlign    = 3 // alignment over tolerance, timestamp regression, duration mismatch
	ExitMissing  = 4 // missing or corrupt segments, count mismatch
	ExitManifest = 5 // manifest parse failure, XML round-trip mismatch
)

// Stable machine-readable error codes.
const (
	CodeParam            = "PARAM_ERROR"
	CodeOutputExists     = "OUTPUT_EXISTS"
	CodeOverflow         = "OVERFLOW"
	CodeAlignExceeded    = "ALIGNMENT_EXCEEDED"
	CodeTimestampRegress = "TIMESTAMP_REGRESSION"
	CodeDurationMismatch = "DURATION_MISMATCH"
	CodeMissingSegment   = "MISSING_SEGMENT"
	CodeSegmentCount     = "SEGMENT_COUNT_MISMATCH"
	CodeSegmentCorrupt   = "SEGMENT_CORRUPT"
	CodeManifestParse    = "MANIFEST_PARSE_ERROR"
	CodeRoundTrip        = "XML_ROUNDTRIP_MISMATCH"
)

// AppError is a structured, JSON-serialisable failure.
type AppError struct {
	Code    string         `json:"error_code"`
	Exit    int            `json:"exit"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

func (e *AppError) Error() string { return e.Code + ": " + e.Message }

// JSON renders the error as a single-line JSON object.
func (e *AppError) JSON() string {
	b, err := json.Marshal(e)
	if err != nil {
		return `{"error_code":"INTERNAL","exit":1,"message":"json marshal failed"}`
	}
	return string(b)
}

// New builds an AppError.
func New(exit int, code, message string, details map[string]any) *AppError {
	return &AppError{Code: code, Exit: exit, Message: message, Details: details}
}
