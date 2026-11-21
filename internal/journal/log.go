// Package journal implements an append-only write-ahead log for remux stage
// commits. A commit record is only trusted when its input fingerprint, output
// length and prefix digest all agree with the bytes on disk.
package journal

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"

	"mediamerge/internal/model"
)

// Record kinds.
const (
	KindBegin  = "begin"
	KindCommit = "commit"
)

// Record is one durable log entry. InputSHA256 is repeated on every record so
// a log can never be replayed against a different input.
type Record struct {
	Stage       int    `json:"stage"`
	Kind        string `json:"kind"`
	InputSHA256 string `json:"input_sha256"`
	InputSize   int64  `json:"input_size"`
	OutputLen   int64  `json:"output_len"`
	PrefixSHA   string `json:"prefix_sha256,omitempty"`
}

// Log is an open journal writer. Every Append is flushed and fsynced before
// returning, so the record is durable before the stage is considered done.
type Log struct {
	f   *os.File
	enc *json.Encoder
}

// Create truncates any existing journal and returns a writer. The directory is
// created when missing; symlinked paths are rejected by the caller.
func Create(path string) (*Log, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, model.WrapError(model.StageJournal, model.CodeJournal, 0, 0, false, err,
			"create journal directory")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, model.WrapError(model.StageJournal, model.CodeJournal, 0, 0, false, err,
			"create journal")
	}
	return &Log{f: f, enc: json.NewEncoder(f)}, nil
}

// Open appends to an existing journal.
func Open(path string) (*Log, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, model.WrapError(model.StageJournal, model.CodeJournal, 0, 0, false, err,
			"open journal")
	}
	return &Log{f: f, enc: json.NewEncoder(f)}, nil
}

// Append durably writes one record.
func (l *Log) Append(r Record) error {
	if l == nil || l.f == nil {
		return model.NewError(model.StageJournal, model.CodeJournal, 0, 0, false, "journal is closed")
	}
	if err := l.enc.Encode(r); err != nil {
		return model.WrapError(model.StageJournal, model.CodeJournal, r.OutputLen, 0, true, err,
			"encode journal record")
	}
	if err := l.f.Sync(); err != nil {
		return model.WrapError(model.StageJournal, model.CodeJournal, r.OutputLen, 0, true, err,
			"sync journal")
	}
	return nil
}

// Close closes the journal.
func (l *Log) Close() error {
	if l == nil || l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}

// Load parses a journal. A trailing line without a newline is treated as a
// torn write and ignored; every other malformed line is a journal integrity
// error carrying its byte offset.
func Load(path string) ([]Record, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, model.NewError(model.StageJournal, model.CodeJournal, 0, 0, false,
				"journal not found")
		}
		return nil, model.WrapError(model.StageJournal, model.CodeJournal, 0, 0, false, err,
			"read journal")
	}
	if len(data) == 0 {
		return nil, model.NewError(model.StageJournal, model.CodeJournal, 0, 0, false,
			"journal is empty")
	}
	var out []Record
	offset := int64(0)
	for len(data) > 0 {
		nl := bytes.IndexByte(data, '\n')
		var line []byte
		complete := true
		if nl < 0 {
			line = data
			complete = false
		} else {
			line = data[:nl]
			data = data[nl+1:]
		}
		if len(bytes.TrimSpace(line)) != 0 {
			var r Record
			if err := json.Unmarshal(line, &r); err != nil {
				if !complete {
					break // torn final write: ignore
				}
				return nil, model.WrapError(model.StageJournal, model.CodeIntegrity, offset, 0, false, err,
					"malformed journal record")
			}
			out = append(out, r)
		}
		offset += int64(len(line)) + 1
		if !complete {
			break
		}
	}
	if len(out) == 0 {
		return nil, model.NewError(model.StageJournal, model.CodeJournal, 0, 0, false,
			"journal has no complete records")
	}
	return out, nil
}

// Format is a human-readable single line summary used by tests.
func (r Record) Format() string {
	prefix := r.PrefixSHA
	if len(prefix) > 12 {
		prefix = prefix[:12]
	}
	return "stage=" + strconv.Itoa(r.Stage) + " kind=" + r.Kind + " output_len=" + strconv.FormatInt(r.OutputLen, 10) + " prefix=" + prefix
}

// Rewrite atomically replaces a journal with the given records.
func Rewrite(path string, prefix, rest []Record) (*Log, error) {
	all := append(append([]Record(nil), prefix...), rest...)
	tmp := path + ".tmp"
	l, err := Create(tmp)
	if err != nil {
		return nil, err
	}
	for _, r := range all {
		if err := l.Append(r); err != nil {
			l.Close()
			return nil, err
		}
	}
	if err := l.Close(); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, path); err != nil {
		return nil, model.WrapError(model.StageJournal, model.CodeJournal, 0, 0, false, err,
			"replace journal")
	}
	return Open(path)
}
