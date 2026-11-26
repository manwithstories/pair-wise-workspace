// Package txn implements write-ahead logging and atomic, replayable commits.
// Every pipeline stage appends a checksummed WAL record before its result is
// committed; on restart the WAL is replayed and any uncommitted or corrupted
// tail is truncated so only fully committed boundaries survive.
package txn

import (
	"encoding/binary"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"strings"

	"mediamix/internal/clock"
)

// Record kinds.
const (
	KindData   uint8 = 1 // fragment payload staged for commit
	KindCommit uint8 = 2 // commit boundary referencing prior record IDs
	KindAbort  uint8 = 3 // explicit rollback marker
)

// magic and framing constants.
const (
	magic        = "MMWAL01"
	headerLen    = 24 // magic(7)+kind(1)+id(8)+datalen(4)+crc(4)
	walFileName  = "mediamix.wal"
	commitSuffix = ".commit"
)

// Record is one WAL entry.
type Record struct {
	Kind  uint8
	ID    uint64
	Data  []byte
	Index int64 // byte offset of the record header in the file
}

// WAL is an append-only, CRC-protected log.
type WAL struct {
	path string
	f    *os.File
	next uint64
}

func walError(code clock.ErrCode, detail string, cause error) *clock.MediaError {
	return clock.WrapError(clock.StageWAL, code, 0, false, detail, cause)
}

// OpenWAL opens (or creates) the log inside dir and validates any existing
// content. A corrupted or partial tail is truncated back to the last valid
// record boundary; the number of dropped bytes is reported.
func OpenWAL(dir string) (*WAL, int64, error) {
	if err := validateDir(dir); err != nil {
		return nil, 0, err
	}
	path := filepath.Join(dir, walFileName)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, 0, walError(clock.ErrInvalidPath, "open wal", err)
	}
	w := &WAL{path: path, f: f, next: 1}
	valid, dropped, err := w.scan()
	if err != nil {
		f.Close()
		return nil, 0, err
	}
	w.next = uint64(valid) + 1
	if dropped > 0 {
		if err := f.Truncate(valid); err != nil {
			f.Close()
			return nil, 0, walError(clock.ErrWALCorrupt, "truncate corrupt tail", err)
		}
		if _, err := f.Seek(valid, io.SeekStart); err != nil {
			f.Close()
			return nil, 0, walError(clock.ErrWALCorrupt, "seek after truncate", err)
		}
	}
	return w, dropped, nil
}

// scan walks every record, returning the byte offset just past the last
// valid record and the number of trailing invalid bytes.
func (w *WAL) scan() (validEnd int64, dropped int64, err error) {
	if _, err := w.f.Seek(0, io.SeekStart); err != nil {
		return 0, 0, walError(clock.ErrWALCorrupt, "rewind wal", err)
	}
	buf := make([]byte, headerLen)
	var off int64
	for {
		n, rerr := io.ReadFull(w.f, buf)
		if rerr == io.EOF {
			break
		}
		if rerr == io.ErrUnexpectedEOF {
			return off, dropped + int64(n), nil
		}
		if rerr != nil {
			return 0, 0, walError(clock.ErrWALCorrupt, "read header", rerr)
		}
		if string(buf[:len(magic)]) != magic {
			return off, dropped + countRemaining(w.f), nil
		}
		kind := buf[7]
		dlen := int(binary.BigEndian.Uint32(buf[16:20]))
		wantCRC := binary.BigEndian.Uint32(buf[20:24])
		if kind < KindData || kind > KindAbort || dlen < 0 {
			return off, dropped + countRemaining(w.f), nil
		}
		data := make([]byte, dlen)
		dn, derr := io.ReadFull(w.f, data)
		if derr != nil {
			return off, dropped + int64(dn), nil
		}
		if crc32.ChecksumIEEE(append(buf[:20], data...)) != wantCRC {
			return off, dropped + countRemaining(w.f), nil
		}
		off += int64(headerLen + dlen)
		dropped = 0
	}
	return off, 0, nil
}

func countRemaining(f *os.File) int64 {
	pos, _ := f.Seek(0, io.SeekCurrent)
	end, _ := f.Seek(0, io.SeekEnd)
	_, _ = f.Seek(pos, io.SeekStart)
	return end - pos
}

// Append writes one record and fsyncs it before returning, so a crash never
// leaves a record half-visible to a later scan.
func (w *WAL) Append(kind uint8, data []byte) (Record, error) {
	if len(data) > 0x7FFFFFFF {
		return Record{}, walError(clock.ErrLimitExceeded, "wal record too large", nil)
	}
	id := w.next
	w.next++
	header := make([]byte, headerLen)
	copy(header[:len(magic)], magic)
	header[7] = kind
	binary.BigEndian.PutUint64(header[8:16], id)
	binary.BigEndian.PutUint32(header[16:20], uint32(len(data)))
	crc := crc32.ChecksumIEEE(append(header[:20], data...))
	binary.BigEndian.PutUint32(header[20:24], crc)

	if _, err := w.f.Seek(0, io.SeekEnd); err != nil {
		return Record{}, walError(clock.ErrWALCorrupt, "seek append", err)
	}
	if _, err := w.f.Write(header); err != nil {
		return Record{}, walError(clock.ErrWALCorrupt, "write header", err)
	}
	if len(data) > 0 {
		if _, err := w.f.Write(data); err != nil {
			return Record{}, walError(clock.ErrWALCorrupt, "write data", err)
		}
	}
	if err := w.f.Sync(); err != nil {
		return Record{}, walError(clock.ErrWALCorrupt, "fsync wal", err)
	}
	return Record{Kind: kind, ID: id, Data: data}, nil
}

// Replay returns all intact records in order.
func (w *WAL) Replay() ([]Record, error) {
	if _, err := w.f.Seek(0, io.SeekStart); err != nil {
		return nil, walError(clock.ErrWALCorrupt, "rewind for replay", err)
	}
	var out []Record
	buf := make([]byte, headerLen)
	for {
		_, rerr := io.ReadFull(w.f, buf)
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return nil, walError(clock.ErrWALCorrupt, "replay header", rerr)
		}
		dlen := int(binary.BigEndian.Uint32(buf[16:20]))
		data := make([]byte, dlen)
		if _, err := io.ReadFull(w.f, data); err != nil {
			return nil, walError(clock.ErrWALCorrupt, "replay data", err)
		}
		out = append(out, Record{
			Kind: buf[7],
			ID:   binary.BigEndian.Uint64(buf[8:16]),
			Data: data,
		})
	}
	return out, nil
}

// Close releases the log file.
func (w *WAL) Close() error {
	if w == nil || w.f == nil {
		return nil
	}
	err := w.f.Close()
	w.f = nil
	return err
}

// Path returns the on-disk WAL location.
func (w *WAL) Path() string { return w.path }

// validateDir rejects empty or traversal-style paths so output cannot be
// redirected outside the workspace by a crafted argument.
func validateDir(dir string) error {
	if strings.TrimSpace(dir) == "" {
		return clock.NewError(clock.StageCommit, clock.ErrInvalidPath, 0, false, "empty directory")
	}
	clean := filepath.Clean(dir)
	for _, part := range strings.Split(clean, string(filepath.Separator)) {
		if part == ".." {
			return clock.NewError(clock.StageCommit, clock.ErrInvalidPath, 0, false,
				"path traversal rejected: "+dir)
		}
	}
	info, err := os.Stat(clean)
	if err != nil {
		if os.IsNotExist(err) {
			return clock.NewError(clock.StageCommit, clock.ErrInvalidPath, 0, false,
				"directory does not exist: "+clean)
		}
		return clock.NewError(clock.StageCommit, clock.ErrInvalidPath, 0, false, "stat dir: "+err.Error())
	}
	if !info.IsDir() {
		return clock.NewError(clock.StageCommit, clock.ErrInvalidPath, 0, false,
			"path is not a directory: "+clean)
	}
	return nil
}
