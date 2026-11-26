// Package recovery implements the local chunk index and checkpoints that
// make interrupted runs resumable. Committed chunks are never re-committed
// and uncommitted chunks are never treated as done.
package recovery

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"os"
	"strconv"
)

// IndexVersion is the on-disk index format version.
const IndexVersion = 1

// Record is one index line. Type is "header", "chunk" or "done".
type Record struct {
	Type          string `json:"type"`
	Version       int    `json:"version,omitempty"`
	ParamsDigest  string `json:"params_digest,omitempty"`
	InputDigest   string `json:"input_digest,omitempty"`
	ChunkSize     int    `json:"chunk_size,omitempty"`
	Chunk         int    `json:"chunk,omitempty"`
	FirstSeq      uint32 `json:"first_seq,omitempty"`
	LastSeq       uint32 `json:"last_seq,omitempty"`
	OutDigest     string `json:"out_digest,omitempty"`
	Segments      int    `json:"segments,omitempty"`
	Run           int    `json:"run,omitempty"`
	ResultsDigest string `json:"results_digest,omitempty"`
	// Next and Dur checkpoint the per-track timeline state at chunk end so
	// a resumed run restores the exact shared timebase.
	Next map[string]int64 `json:"next,omitempty"`
	Dur  map[string]int64 `json:"dur,omitempty"`
}

// ParamsDigest binds an index to the run parameters and input content.
func ParamsDigest(inputDigest string, chunkSize int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("relaycheck/v%d|chunk=%d|input=%s",
		IndexVersion, chunkSize, inputDigest)))
	return hex.EncodeToString(sum[:])
}

// Index is an append-only JSONL file; every line carries a CRC32 so a
// torn write at the tail is detected and discarded on open.
type Index struct {
	path        string
	f           *os.File
	Header      *Record
	Chunks      map[int]Record
	Done        *Record
	CorruptTail bool
}

func encodeRecord(r *Record) []byte {
	payload, _ := json.Marshal(r)
	return []byte(fmt.Sprintf("%08x %s\n", crc32.ChecksumIEEE(payload), payload))
}

func decodeRecord(line []byte) (*Record, error) {
	i := bytes.IndexByte(line, ' ')
	if i != 8 {
		return nil, fmt.Errorf("malformed index line")
	}
	want, err := strconv.ParseUint(string(line[:i]), 16, 32)
	if err != nil {
		return nil, err
	}
	if crc32.ChecksumIEEE(line[i+1:]) != uint32(want) {
		return nil, fmt.Errorf("index record checksum mismatch")
	}
	var r Record
	if err := json.Unmarshal(line[i+1:], &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// Open loads (or creates) an index. A corrupt tail — e.g. from a crashed
// writer — is truncated and reported via CorruptTail; a corrupt record
// followed by more data is an error.
func Open(path string) (*Index, error) {
	x := &Index{path: path, Chunks: map[int]Record{}}
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	good := 0
	if len(data) > 0 {
		s := bufio.NewScanner(bytes.NewReader(data))
		s.Buffer(make([]byte, 1<<20), 1<<20)
		for s.Scan() {
			line := s.Bytes()
			if len(line) == 0 {
				continue
			}
			rec, err := decodeRecord(line)
			if err != nil {
				x.CorruptTail = true
				break
			}
			switch rec.Type {
			case "header":
				if x.Header != nil {
					return nil, fmt.Errorf("duplicate index header")
				}
				x.Header = rec
			case "chunk":
				x.Chunks[rec.Chunk] = *rec
			case "done":
				x.Done = rec
			default:
				return nil, fmt.Errorf("unknown index record %q", rec.Type)
			}
			good += len(line) + 1
		}
		if x.CorruptTail {
			if err := os.Truncate(path, int64(good)); err != nil {
				return nil, err
			}
		}
	}
	x.f, err = os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	return x, nil
}

// Append writes one record and fsyncs before returning.
func (x *Index) Append(r *Record) error {
	if _, err := x.f.Write(encodeRecord(r)); err != nil {
		return err
	}
	if err := x.f.Sync(); err != nil {
		return err
	}
	switch r.Type {
	case "header":
		x.Header = r
	case "chunk":
		x.Chunks[r.Chunk] = *r
	case "done":
		x.Done = r
	}
	return nil
}

// ResumedChunks counts chunks committed by a run other than the first.
func (x *Index) ResumedChunks() int {
	n := 0
	for _, c := range x.Chunks {
		if c.Run > 1 {
			n++
		}
	}
	return n
}

// Close closes the index file.
func (x *Index) Close() error {
	return x.f.Close()
}
