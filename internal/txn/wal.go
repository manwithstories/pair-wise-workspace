// Package txn wraps every pipeline stage in a write-ahead log with
// checkpoints. A stage commits atomically only after its input and output
// hashes match; a corrupted stage rolls back to the last verified checkpoint.
// A truncated log, a full disk or an interrupted commit must never leave a
// half-written final manifest behind.
package txn

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"mediarun/internal/clock"
)

// RecordType enumerates WAL record kinds.
type RecordType string

const (
	RecRunStart    RecordType = "run-start"
	RecStageBegin  RecordType = "stage-begin"
	RecStageData   RecordType = "stage-data"
	RecCheckpoint  RecordType = "checkpoint"
	RecStageCommit RecordType = "stage-commit"
	RecRollback    RecordType = "rollback"
	RecRunEnd      RecordType = "run-end"
)

// Record is one durable line in the log. Sum makes truncation detectable:
// a record whose body does not hash to Sum is discarded during replay.
type Record struct {
	Type       RecordType `json:"type"`
	Seq        uint64     `json:"seq"`
	RunID      string     `json:"run_id"`
	Stage      string     `json:"stage"`
	InputHash  string     `json:"input_hash,omitempty"`
	OutputHash string     `json:"output_hash,omitempty"`
	Cursor     string     `json:"cursor,omitempty"`
	Temp       string     `json:"temp,omitempty"`
	Final      string     `json:"final,omitempty"`
	Detail     string     `json:"detail,omitempty"`
	Sum        string     `json:"sum"`
}

// computeSum hashes the canonical JSON of the record without its own Sum.
func computeSum(r Record) string {
	type shadow Record
	s := shadow(r)
	s.Sum = ""
	b, _ := json.Marshal(s)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// Sign fills in the record hash.
func (r *Record) Sign() { r.Sum = computeSum(*r) }

// Verify checks the record hash.
func (r Record) Verify() bool { return r.Sum == computeSum(r) }

// Log is an append-only WAL. It is safe to reopen after a crash.
type Log struct {
	path  string
	f     *os.File
	seq   uint64
	runID string
	// FailAfter injects a write failure after that many accepted appends.
	FailAfter int
	appends   int
	// maxBytes caps the log size so a corrupt writer cannot fill the disk.
	maxBytes int64
	written  int64
}

// OpenLog opens or creates the WAL at path.
func OpenLog(path, runID string) (*Log, error) {
	l := &Log{path: path, runID: runID, maxBytes: 256 << 20}
	if err := l.reload(); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, clock.Wrap(clock.KindRecovery, clock.CodeCheckpoint, path, err)
	}
	l.f = f
	return l, nil
}

// reload replays existing records to restore seq and detect truncation.
func (l *Log) reload() error {
	st, err := os.Stat(l.path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	l.written = st.Size()
	f, err := os.Open(l.path)
	if err != nil {
		return err
	}
	defer f.Close()
	recs, err := Replay(f)
	if err != nil {
		return err
	}
	if len(recs) > 0 {
		l.seq = recs[len(recs)-1].Seq
	}
	return nil
}

// Append signs and durably writes a record.
func (l *Log) Append(r Record) error {
	if l.FailAfter > 0 && l.appends >= l.FailAfter {
		return clock.New(clock.KindRecovery, clock.CodeDiskFull, l.path,
			"simulated write failure")
	}
	l.seq++
	r.Seq = l.seq
	r.RunID = l.runID
	r.Sign()
	b, err := json.Marshal(r)
	if err != nil {
		return clock.Wrap(clock.KindRecovery, clock.CodeCheckpoint, l.path, err)
	}
	b = append(b, '\n')
	if l.written+int64(len(b)) > l.maxBytes {
		return clock.New(clock.KindRecovery, clock.CodeDiskFull, l.path,
			"log exceeds size ceiling")
	}
	if _, err := l.f.Write(b); err != nil {
		return clock.Wrap(clock.KindRecovery, clock.CodeDiskFull, l.path, err)
	}
	if err := l.f.Sync(); err != nil {
		return clock.Wrap(clock.KindRecovery, clock.CodeDiskFull, l.path, err)
	}
	l.written += int64(len(b))
	l.appends++
	return nil
}

// Close flushes and closes the WAL.
func (l *Log) Close() error {
	if l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}

// Replay reads records, silently discarding any truncated tail. A record that
// fails its own hash check is treated as end-of-log, which is exactly what a
// crash mid-append looks like.
func Replay(r io.Reader) ([]Record, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 8<<20)
	var out []Record
	var last uint64
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var rec Record
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			break // truncated JSON: stop here
		}
		if !rec.Verify() {
			break
		}
		if last != 0 && rec.Seq != last+1 {
			break // sequence hole: truncated or tampered log
		}
		last = rec.Seq
		out = append(out, rec)
	}
	return out, nil
}

// ReplayFile replays a WAL file from disk.
func ReplayFile(path string) ([]Record, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, clock.Wrap(clock.KindRecovery, clock.CodeCheckpoint, path, err)
	}
	defer f.Close()
	return Replay(f)
}

// LastCheckpoint finds the highest sequence checkpoint that marks a stage.
func LastCheckpoint(recs []Record) (Record, bool) {
	for i := len(recs) - 1; i >= 0; i-- {
		if recs[i].Type == RecCheckpoint {
			return recs[i], true
		}
	}
	return Record{}, false
}

// LastCommit finds the highest sequence stage commit.
func LastCommit(recs []Record) (Record, bool) {
	for i := len(recs) - 1; i >= 0; i-- {
		if recs[i].Type == RecStageCommit {
			return recs[i], true
		}
	}
	return Record{}, false
}

// TruncateTail simulates a crash that lost the final partial append. It
// rewrites the file keeping only intact records.
func TruncateTail(path string) error {
	recs, err := ReplayFile(path)
	if err != nil {
		return err
	}
	var b strings.Builder
	for _, r := range recs {
		js, _ := json.Marshal(r)
		b.Write(js)
		b.WriteByte('\n')
	}
	tmp := path + ".replay"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// HashBytes returns the hex SHA-256 of b.
func HashBytes(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// HashFile hashes a file's contents.
func HashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", clock.Wrap(clock.KindRecovery, clock.CodeHashMismatch, path, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", clock.Wrap(clock.KindRecovery, clock.CodeHashMismatch, path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// EnsureDir creates a directory, including parents, if absent.
func EnsureDir(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return clock.Wrap(clock.KindRecovery, clock.CodeCheckpoint, path, err)
	}
	return nil
}

// String renders a record for the terminal log.
func (r Record) String() string {
	return fmt.Sprintf("#%d %s/%s in=%s out=%s",
		r.Seq, r.Type, r.Stage, short(r.InputHash), short(r.OutputHash))
}

func short(h string) string {
	if h == "" {
		return "-"
	}
	if len(h) > 12 {
		return h[:12]
	}
	return h
}
