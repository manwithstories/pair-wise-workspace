// Package index implements the local write-ahead journal and the segment
// checkpoint. Only fully validated segment groups are committed; a torn
// tail record after a crash is detected by its SHA-256 and ignored.
package index

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"

	"avpipe/internal/media"
)

const (
	journalName = "journal.avp"
	recordMagic = "AVPR"
	recordSize  = 256
	headerLen   = 8 // magic(4) + payload length(4)
	typeLen     = 1 // explicit record-type byte before the payload
	// payloadCap bounds the variable part so every record stays fixed width.
	payloadCap = recordSize - headerLen - typeLen - sha256.Size // 219 bytes
)

// Record type tags.
const (
	RecCheckpoint = 1
	RecGroup      = 2
)

// Entry is one checksummed journal record.
type Entry struct {
	Type    uint8
	Payload []byte
}

// Journal is an append-only file of fixed-size, checksummed records.
type Journal struct {
	file *os.File
	path string
}

// OpenJournal opens or creates the journal inside dir and replays intact
// records. A truncated or corrupted tail is truncated away safely.
func OpenJournal(dir string) (*Journal, []Entry, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, nil, media.NewError(media.ClassIndex, "index", "mkdir failed: "+err.Error(), 0, false)
	}
	path := filepath.Join(dir, journalName)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, nil, media.NewError(media.ClassIndex, "index", "open failed: "+err.Error(), 0, false)
	}
	j := &Journal{file: f, path: path}
	entries, goodBytes, err := replay(f)
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	if err := f.Truncate(int64(goodBytes)); err != nil {
		f.Close()
		return nil, nil, media.NewError(media.ClassIndex, "index", "truncate torn tail failed: "+err.Error(), 0, false)
	}
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		f.Close()
		return nil, nil, media.NewError(media.ClassIndex, "index", "seek failed: "+err.Error(), 0, false)
	}
	return j, entries, nil
}

// replay scans fixed-size records and returns intact entries plus the byte
// offset at which the first invalid record begins.
func replay(f *os.File) ([]Entry, int, error) {
	info, err := f.Stat()
	if err != nil {
		return nil, 0, media.NewError(media.ClassIndex, "index", "stat failed", 0, false)
	}
	total := info.Size()
	raw := make([]byte, recordSize)
	var entries []Entry
	good := 0
	for off := int64(0); off+recordSize <= total; off += recordSize {
		if _, err := io.ReadFull(f, raw); err != nil {
			break
		}
		e, ok := decodeRecord(raw)
		if !ok {
			break // torn tail: ignore everything from here
		}
		entries = append(entries, e)
		good += recordSize
	}
	return entries, good, nil
}

// Append writes and fsyncs one record. Payloads larger than payloadCap are
// rejected so the on-disk layout stays fixed width.
func (j *Journal) Append(e Entry) error {
	if len(e.Payload) > payloadCap {
		return media.NewError(media.ClassIndex, "index", "journal payload too large", 0, false)
	}
	raw := make([]byte, recordSize)
	copy(raw[0:4], recordMagic)
	binary.LittleEndian.PutUint32(raw[4:8], uint32(len(e.Payload)))
	raw[headerLen] = e.Type
	copy(raw[headerLen+typeLen:headerLen+typeLen+len(e.Payload)], e.Payload)
	sum := sha256.Sum256(raw[:headerLen+typeLen+len(e.Payload)])
	copy(raw[recordSize-sha256.Size:], sum[:])
	if _, err := j.file.Write(raw); err != nil {
		return media.NewError(media.ClassIndex, "index", "write failed: "+err.Error(), 0, false)
	}
	return j.file.Sync()
}

// Close releases the journal file.
func (j *Journal) Close() error { return j.file.Close() }

// Path returns the journal location.
func (j *Journal) Path() string { return j.path }

func decodeRecord(raw []byte) (Entry, bool) {
	if len(raw) != recordSize || string(raw[0:4]) != recordMagic {
		return Entry{}, false
	}
	n := binary.LittleEndian.Uint32(raw[4:8])
	if int(n) > payloadCap {
		return Entry{}, false
	}
	end := headerLen + typeLen + int(n)
	stored := raw[recordSize-sha256.Size:]
	sum := sha256.Sum256(raw[:end])
	if !bytesEqual(sum[:], stored) {
		return Entry{}, false
	}
	return Entry{
		Type:    raw[headerLen],
		Payload: append([]byte(nil), raw[headerLen+typeLen:end]...),
	}, true
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ErrNoCheckpoint means no committed checkpoint exists yet.
var ErrNoCheckpoint = errors.New("no checkpoint recorded")
