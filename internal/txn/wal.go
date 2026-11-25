// Package txn provides write-ahead logging and atomic artifact commits.
package txn

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
)

// Record is one checksummed WAL stage.
type Record struct {
	Seq      int64  `json:"seq"`
	Stage    string `json:"stage"`
	TxnID    string `json:"txn_id"`
	Name     string `json:"name"`
	SHA256   string `json:"sha256"`
	Size     int    `json:"size"`
	Checksum string `json:"checksum"`
}

// WAL is an append-only JSON-line write-ahead log.
type WAL struct {
	path string
	f    *os.File
	seq  int64
}

// ReplayResult separates validated records from an invalid/torn tail.
type ReplayResult struct {
	Records     []Record
	InvalidLine int
	TornTail    bool
}

// OpenWAL opens or creates a WAL and initializes its sequence from valid data.
func OpenWAL(path string) (*WAL, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	res, err := ReplayWAL(path)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	var seq int64
	for _, r := range res.Records {
		if r.Seq > seq {
			seq = r.Seq
		}
	}
	return &WAL{path: path, f: f, seq: seq}, nil
}

// Append writes and fsyncs one checksummed record.
func (w *WAL) Append(stage, txnID, name, sha string, size int) (Record, error) {
	w.seq++
	r := Record{Seq: w.seq, Stage: stage, TxnID: txnID, Name: name, SHA256: sha, Size: size}
	r.Checksum = RecordChecksum(r)
	b, err := json.Marshal(r)
	if err != nil {
		return Record{}, err
	}
	b = append(b, '\n')
	if _, err := w.f.Write(b); err != nil {
		return Record{}, err
	}
	if err := w.f.Sync(); err != nil {
		return Record{}, err
	}
	return r, nil
}

// Close syncs and closes the WAL.
func (w *WAL) Close() error {
	if w == nil || w.f == nil {
		return nil
	}
	if err := w.f.Sync(); err != nil {
		return err
	}
	return w.f.Close()
}

// Len returns the highest sequence number.
func (w *WAL) Len() int64 { return w.seq }

// RecordChecksum hashes all record fields except Checksum itself.
func RecordChecksum(r Record) string {
	r.Checksum = ""
	b, _ := json.Marshal(r)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// VerifyChecksum reports whether r's checksum matches its fields.
func (r Record) VerifyChecksum() bool {
	return subtleEqual(RecordChecksum(r), r.Checksum)
}

func subtleEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var v byte
	for i := 0; i < len(a); i++ {
		v |= a[i] ^ b[i]
	}
	return v == 0
}

// ReplayWAL reads only checksum-valid records. A partial final line caused by
// a crash is reported as TornTail and ignored. A complete corrupt line stops
// replay at that boundary.
func ReplayWAL(path string) (ReplayResult, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return ReplayResult{}, nil
	}
	if err != nil {
		return ReplayResult{}, err
	}
	defer f.Close()

	data, err := io.ReadAll(f)
	if err != nil {
		return ReplayResult{}, err
	}
	var out ReplayResult
	if len(data) == 0 {
		return out, nil
	}
	lines := bytes.Split(data, []byte{'\n'})
	// Data ending in newline produces an empty final element.
	for i, line := range lines {
		if len(line) == 0 {
			if i == len(lines)-1 && len(data) > 0 && data[len(data)-1] != '\n' {
				out.TornTail = true
			}
			continue
		}
		var r Record
		if err := json.Unmarshal(line, &r); err != nil || !r.VerifyChecksum() {
			out.InvalidLine = i + 1
			// A final line without newline is a torn append, not an invariant
			// violation. Earlier invalid complete lines are also not restored.
			if i == len(lines)-1 || (i == len(lines)-2 && len(lines[len(lines)-1]) == 0) {
				out.TornTail = true
			}
			break
		}
		out.Records = append(out.Records, r)
	}
	return out, nil
}

// SHA256File returns the hex SHA-256 and size of an existing file.
func SHA256File(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", n, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}
