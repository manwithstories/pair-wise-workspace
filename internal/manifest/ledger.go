// Package manifest creates and checks deterministic cross-track event ledgers
// and the resumable run journal.
package manifest

import (
	"bytes"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strconv"

	"archivectl/internal/container"
	"archivectl/internal/timeline"
)

// LedgerEntry is one cross-track timed marker.
type LedgerEntry struct {
	Kind        string
	TrackID     uint32
	SampleIndex int
	TimeNS      int64
	TimeMS      float64
	RawTime     int64
	Timescale   uint32
	Label       string
	SHA256      string
}

var header = []string{
	"kind", "track_id", "sample_index", "time_ns", "time_ms",
	"raw_time", "timescale", "sha256", "label",
}

// BuildEntries converts container events to stable ledger rows.
func BuildEntries(events []container.Event) []LedgerEntry {
	out := make([]LedgerEntry, 0, len(events))
	for _, e := range events {
		out = append(out, LedgerEntry{
			Kind: e.Kind, TrackID: e.TrackID, SampleIndex: e.SampleIndex,
			TimeNS: e.TimeNS, TimeMS: float64(e.TimeNS) / 1e6,
			RawTime: e.RawTime, Timescale: e.Timescale, Label: e.Label, SHA256: e.Hash,
		})
	}
	return out
}

// EncodeCSV returns the deterministic CSV ledger bytes.
func EncodeCSV(entries []LedgerEntry) ([]byte, error) {
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	if err := w.Write(header); err != nil {
		return nil, err
	}
	for _, e := range entries {
		row := []string{
			e.Kind,
			strconv.FormatUint(uint64(e.TrackID), 10),
			strconv.Itoa(e.SampleIndex),
			strconv.FormatInt(e.TimeNS, 10),
			fmt.Sprintf("%.6f", e.TimeMS),
			strconv.FormatInt(e.RawTime, 10),
			strconv.FormatUint(uint64(e.Timescale), 10),
			e.SHA256,
			e.Label,
		}
		if err := w.Write(row); err != nil {
			return nil, err
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// WriteCSV writes a deterministic CSV ledger.
func WriteCSV(path string, entries []LedgerEntry) error {
	b, err := EncodeCSV(entries)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return timeline.Wrap(timeline.CodeInvalidArgument, "create manifest", err)
	}
	return nil
}

// ReadCSV reads and validates a ledger.
func ReadCSV(path string) ([]LedgerEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, timeline.Wrap(timeline.CodeInvalidArgument, "open manifest", err)
	}
	defer f.Close()
	r := csv.NewReader(f)
	row, err := r.Read()
	if err != nil {
		return nil, timeline.Wrap(timeline.CodeCorruptBox, "read manifest header", err)
	}
	if len(row) != len(header) || row[0] != "kind" {
		return nil, timeline.New(timeline.CodeIntegrity, "read manifest", "bad header")
	}
	var out []LedgerEntry
	for {
		row, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, timeline.Wrap(timeline.CodeCorruptBox, "read manifest row", err)
		}
		e, err := parseRow(row)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}

func parseRow(row []string) (LedgerEntry, error) {
	if len(row) != len(header) {
		return LedgerEntry{}, timeline.New(timeline.CodeIntegrity, "parse manifest row", "wrong column count")
	}
	var e LedgerEntry
	var err error
	e.Kind = row[0]
	if v, err := strconv.ParseUint(row[1], 10, 32); err == nil {
		e.TrackID = uint32(v)
	} else {
		return e, err
	}
	if e.SampleIndex, err = strconv.Atoi(row[2]); err != nil {
		return e, err
	}
	if e.TimeNS, err = strconv.ParseInt(row[3], 10, 64); err != nil {
		return e, err
	}
	if e.TimeMS, err = strconv.ParseFloat(row[4], 64); err != nil {
		return e, err
	}
	if e.RawTime, err = strconv.ParseInt(row[5], 10, 64); err != nil {
		return e, err
	}
	if v, err := strconv.ParseUint(row[6], 10, 32); err == nil {
		e.Timescale = uint32(v)
	} else {
		return e, err
	}
	e.SHA256 = row[7]
	e.Label = row[8]
	return e, nil
}

// CheckEvents compares ledger rows to demuxed events and returns the maximum
// millisecond discrepancy.
func CheckEvents(entries []LedgerEntry, events []container.Event) (maxErrorMS float64, hashMismatch int, err error) {
	if len(entries) != len(events) {
		return 0, 0, &timeline.Error{Code: timeline.CodeIntegrity, Op: "check manifest", Offset: -1,
			Detail: fmt.Sprintf("row count %d != event count %d", len(entries), len(events))}
	}
	for i, e := range entries {
		ev := events[i]
		if e.Kind != ev.Kind || e.TrackID != ev.TrackID || e.SampleIndex != ev.SampleIndex {
			return maxErrorMS, hashMismatch, &timeline.Error{Code: timeline.CodeIntegrity, Op: "check manifest identity", Offset: -1,
				Detail: fmt.Sprintf("row %d attribution mismatch", i)}
		}
		reportedNS := int64(e.TimeMS * 1_000_000)
		diffNS := abs64(reportedNS - ev.TimeNS)
		errMS := float64(diffNS) / 1e6
		if errMS > maxErrorMS {
			maxErrorMS = errMS
		}
		if e.TimeNS != ev.TimeNS {
			return maxErrorMS, hashMismatch, &timeline.Error{Code: timeline.CodeIntegrity, Op: "check manifest time_ns", Offset: -1,
				Detail: fmt.Sprintf("row %d time_ns %d != %d", i, e.TimeNS, ev.TimeNS)}
		}
		if e.SHA256 != ev.Hash {
			hashMismatch++
		}
	}
	if hashMismatch > 0 {
		return maxErrorMS, hashMismatch, timeline.New(timeline.CodeIntegrity, "check manifest hash", "event payload hash mismatch")
	}
	if maxErrorMS > 1.0 {
		return maxErrorMS, hashMismatch, timeline.New(timeline.CodeIntegrity, "check manifest timing", "manifest error exceeds 1ms")
	}
	return maxErrorMS, 0, nil
}

func abs64(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}

// HashFile returns the SHA-256 of a file's bytes.
func HashFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", timeline.Wrap(timeline.CodeInvalidArgument, "hash file", err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// HashBytes returns hex SHA-256 of bytes.
func HashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
